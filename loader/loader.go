/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2019 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package loader

import (
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
)

// SourceData wraps a source file; data and filename.
type SourceData struct {
	Data []byte
	URL  *url.URL
}

type loaderFunc func(path string, parts []string) (string, error)

//nolint: gochecknoglobals
var (
	loaders = []struct {
		name string
		fn   loaderFunc
		expr *regexp.Regexp
	}{
		{"cdnjs", cdnjs, regexp.MustCompile(`^cdnjs.com/libraries/([^/]+)(?:/([(\d\.)]+-?[^/]*))?(?:/(.*))?$`)},
		{"github", github, regexp.MustCompile(`^github.com/([^/]+)/([^/]+)/(.*)$`)},
	}
	httpsSchemeCouldntBeLoadedMsg = `The moduleSpecifier "%s" couldn't be retrieved from` +
		` the resolved url "%s". Error : "%s"`
	fileSchemeCouldntBeLoadedMsg = `The moduleSpecifier "%s" couldn't be found on ` +
		`local disk. Make sure that you've specified the right path to the file. If you're ` +
		`running k6 using the Docker image make sure you have mounted the ` +
		`local directory (-v /local/path/:/inside/docker/path) containing ` +
		`your script and modules so that they're accessible by k6 from ` +
		`inside of the container, see ` +
		`https://docs.k6.io/v1.0/docs/modules#section-using-local-modules-with-docker.`
	errNoLoaderMatched = errors.New("no loader matched")
)

// Resolve a relative path to an absolute one.
func Resolve(pwd *url.URL, moduleSpecifier string) (*url.URL, error) {
	if moduleSpecifier == "" {
		return nil, errors.New("local or remote path required")
	}

	if moduleSpecifier[0] == '.' || moduleSpecifier[0] == '/' || filepath.IsAbs(moduleSpecifier) {
		if pwd.Opaque != "" { // this is a loader reference
			parts := strings.SplitN(pwd.Opaque, "/", 2)
			if moduleSpecifier[0] == '/' {
				return &url.URL{Opaque: path.Join(parts[0], moduleSpecifier)}, nil
			}
			return &url.URL{Opaque: path.Join(parts[0], path.Join(path.Dir(parts[1]+"/"), moduleSpecifier))}, nil
		}

		// The file is in format like C:/something/path.js. But this will be decoded as scheme `C`
		// ... which is not what we want we want it to be decode as file:///C:/something/path.js
		if filepath.VolumeName(moduleSpecifier) != "" {
			moduleSpecifier = "/" + moduleSpecifier
		}

		// we always want for the pwd to end in a slash, but filepath/path.Clean strips it so we read
		// it if it's missing
		var finalPwd = pwd
		if pwd.Opaque != "" {
			if !strings.HasSuffix(pwd.Opaque, "/") {
				finalPwd = &url.URL{Opaque: pwd.Opaque + "/"}
			}
		} else if !strings.HasSuffix(pwd.Path, "/") {
			finalPwd = &url.URL{}
			*finalPwd = *pwd
			finalPwd.Path += "/"
		}
		return finalPwd.Parse(moduleSpecifier)
	}

	if strings.Contains(moduleSpecifier, "://") {
		u, err := url.Parse(moduleSpecifier)
		if err != nil {
			return nil, err
		}
		if u.Scheme != "file" && u.Scheme != "https" {
			return nil,
				errors.Errorf("only supported schemes for imports are file and https, %s has `%s`",
					moduleSpecifier, u.Scheme)
		}
		if u.Scheme == "file" && pwd.Scheme == "https" {
			return nil, errors.Errorf("origin (%s) not allowed to load local file: %s", pwd, moduleSpecifier)
		}
		return u, err
	}
	// here we only care if a loader is pickable, if it is and later there is an error in the loading
	// from it we don't want to try another resolve
	_, loader, _ := pickLoader(moduleSpecifier)
	if loader == nil {
		u, err := url.Parse("https://" + moduleSpecifier)
		if err != nil {
			return nil, err
		}
		u.Scheme = ""
		return u, nil
	}
	return &url.URL{Opaque: moduleSpecifier}, nil
}

// Dir returns the directory for the path.
func Dir(old *url.URL) *url.URL {
	if old.Opaque != "" { // loader
		return &url.URL{Opaque: path.Join(old.Opaque, "../")}
	}
	return old.ResolveReference(&url.URL{Path: "./"})
}

// Load loads the provided moduleSpecifier from the given filesystems which are map of afero.Fs
// for a given scheme which is they key of the map. If the scheme is https then a request will
// be made if the files is not found in the map and written to the map.
func Load(
	filesystems map[string]afero.Fs, moduleSpecifier *url.URL, originalModuleSpecifier string,
) (*SourceData, error) {
	log.WithFields(
		log.Fields{
			"moduleSpecifier":          moduleSpecifier,
			"original moduleSpecifier": originalModuleSpecifier,
		}).Debug("Loading...")

	var pathOnFs string
	switch {
	case moduleSpecifier.Opaque != "": // This is loader
		pathOnFs = filepath.Join(afero.FilePathSeparator, moduleSpecifier.Opaque)
	case moduleSpecifier.Scheme == "":
		pathOnFs = path.Clean(moduleSpecifier.String())
	default:
		pathOnFs = path.Clean(moduleSpecifier.String()[len(moduleSpecifier.Scheme)+len(":/"):])
	}
	scheme := moduleSpecifier.Scheme
	if scheme == "" {
		scheme = "https"
	}

	pathOnFs, err := url.PathUnescape(filepath.FromSlash(pathOnFs))
	if err != nil {
		return nil, err
	}

	data, err := afero.ReadFile(filesystems[scheme], pathOnFs)

	if err != nil {
		if os.IsNotExist(err) {
			if scheme == "https" {
				var finalModuleSpecifierURL = &url.URL{}

				switch {
				case moduleSpecifier.Opaque != "": // This is loader
					finalModuleSpecifierURL, err = resolveUsingLoaders(moduleSpecifier.Opaque)
					if err != nil {
						return nil, err
					}
				case moduleSpecifier.Scheme == "":
					log.WithField("url", moduleSpecifier).Warning(
						"A url was resolved but it didn't have scheme. " +
							"This will be deprecated in the future and all remote modules will " +
							"need to explicitly use `https` as scheme")
					*finalModuleSpecifierURL = *moduleSpecifier
					finalModuleSpecifierURL.Scheme = scheme
				default:
					finalModuleSpecifierURL = moduleSpecifier
				}
				var result *SourceData
				result, err = loadRemoteURL(finalModuleSpecifierURL)
				if err != nil {
					return nil, errors.Errorf(httpsSchemeCouldntBeLoadedMsg, originalModuleSpecifier, finalModuleSpecifierURL, err)
				}
				result.URL = moduleSpecifier
				// TODO maybe make an afero.Fs which makes request directly and than use CacheOnReadFs
				// on top of as with the `file` scheme fs
				_ = afero.WriteFile(filesystems[scheme], pathOnFs, result.Data, 0644)
				return result, nil
			}
			return nil, errors.Errorf(fileSchemeCouldntBeLoadedMsg, moduleSpecifier)
		}
		return nil, err
	}

	return &SourceData{URL: moduleSpecifier, Data: data}, nil
}

func resolveUsingLoaders(name string) (*url.URL, error) {
	_, loader, loaderArgs := pickLoader(name)
	if loader != nil {
		urlString, err := loader(name, loaderArgs)
		if err != nil {
			return nil, err
		}
		return url.Parse(urlString)
	}

	return nil, errNoLoaderMatched
}

func loadRemoteURL(u *url.URL) (*SourceData, error) {
	var oldQuery = u.RawQuery
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += "_k6=1"

	data, err := fetch(u.String())

	u.RawQuery = oldQuery
	// If this fails, try to fetch without ?_k6=1 - some sources act weird around unknown GET args.
	if err != nil {
		data, err = fetch(u.String())
		if err != nil {
			return nil, err
		}
	}

	// TODO: Parse the HTML, look for meta tags!!
	// <meta name="k6-import" content="example.com/path/to/real/file.txt" />
	// <meta name="k6-import" content="github.com/myusername/repo/file.txt" />

	return &SourceData{URL: u, Data: data}, nil
}

func pickLoader(path string) (string, loaderFunc, []string) {
	for _, loader := range loaders {
		matches := loader.expr.FindAllStringSubmatch(path, -1)
		if len(matches) > 0 {
			return loader.name, loader.fn, matches[0][1:]
		}
	}
	return "", nil, nil
}

func fetch(u string) ([]byte, error) {
	log.WithField("url", u).Debug("Fetching source...")
	startTime := time.Now()
	res, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != 200 {
		switch res.StatusCode {
		case 404:
			return nil, errors.Errorf("not found: %s", u)
		default:
			return nil, errors.Errorf("wrong status code (%d) for: %s", res.StatusCode, u)
		}
	}

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	log.WithFields(log.Fields{
		"url": u,
		"t":   time.Since(startTime),
		"len": len(data),
	}).Debug("Fetched!")
	return data, nil
}
