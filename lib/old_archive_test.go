package lib

import (
	"archive/tar"
	"bytes"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/loadimpact/k6/lib/fsext"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func dumpMemMapFsToBuf(fs afero.Fs) (*bytes.Buffer, error) {
	var b = bytes.NewBuffer(nil)
	var w = tar.NewWriter(b)
	err := fsext.Walk(fs, afero.FilePathSeparator,
		filepath.WalkFunc(func(filePath string, info os.FileInfo, err error) error {
			if filePath == afero.FilePathSeparator {
				return nil // skip the root
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				return w.WriteHeader(&tar.Header{
					Name:     path.Clean(filepath.ToSlash(filePath)[1:]),
					Mode:     0555,
					Typeflag: tar.TypeDir,
				})
			}
			var data []byte
			data, err = afero.ReadFile(fs, filePath)
			if err != nil {
				return err
			}
			err = w.WriteHeader(&tar.Header{
				Name:     path.Clean(filepath.ToSlash(filePath)[1:]),
				Mode:     0644,
				Size:     int64(len(data)),
				Typeflag: tar.TypeReg,
			})
			if err != nil {
				return err
			}
			_, err = w.Write(data)
			if err != nil {
				return err
			}
			return nil
		}))
	if err != nil {
		return nil, err
	}
	return b, w.Close()
}

func TestOldArchive(t *testing.T) {
	var testCases = map[string]string{
		// map of filename to data for each main file tested
		"github.com/loadimpact/k6/samples/example.js": `github file`,
		"cdnjs.com/packages/Faker":                    `faker file`,
		"C:/something/path2":                          `windows script`,
		"/absolulte/path2":                            `unix script`,
	}
	for filename, data := range testCases {
		filename, data := filename, data
		t.Run(filename, func(t *testing.T) {
			metadata := `{"filename": "` + filename + `"}`
			fs := makeMemMapFs(t, map[string][]byte{
				// files
				"/files/github.com/loadimpact/k6/samples/example.js": []byte(`github file`),
				"/files/cdnjs.com/packages/Faker":                    []byte(`faker file`),
				"/files/example.com/path/to.js":                      []byte(`example.com file`),
				"/files/_/C/something/path":                          []byte(`windows file`),
				"/files/_/absolulte/path":                            []byte(`unix file`),

				// scripts
				"/scripts/github.com/loadimpact/k6/samples/example.js2": []byte(`github script`),
				"/scripts/cdnjs.com/packages/Faker2":                    []byte(`faker script`),
				"/scripts/example.com/path/too.js":                      []byte(`example.com script`),
				"/scripts/_/C/something/path2":                          []byte(`windows script`),
				"/scripts/_/absolulte/path2":                            []byte(`unix script`),
				"/data":                                                 []byte(data),
				"/metadata.json":                                        []byte(metadata),
			})

			buf, err := dumpMemMapFsToBuf(fs)
			require.NoError(t, err)

			var (
				expectedFilesystems = map[string]afero.Fs{
					"file": makeMemMapFs(t, map[string][]byte{
						"/C:/something/path":  []byte(`windows file`),
						"/absolulte/path":     []byte(`unix file`),
						"/C:/something/path2": []byte(`windows script`),
						"/absolulte/path2":    []byte(`unix script`),
					}),
					"https": makeMemMapFs(t, map[string][]byte{
						"/example.com/path/to.js":                       []byte(`example.com file`),
						"/example.com/path/too.js":                      []byte(`example.com script`),
						"/github.com/loadimpact/k6/samples/example.js":  []byte(`github file`),
						"/cdnjs.com/packages/Faker":                     []byte(`faker file`),
						"/github.com/loadimpact/k6/samples/example.js2": []byte(`github script`),
						"/cdnjs.com/packages/Faker2":                    []byte(`faker script`),
					}),
				}
			)

			arc, err := ReadArchive(buf)
			require.NoError(t, err)

			diffMapFilesystems(t, expectedFilesystems, arc.Filesystems)
		})
	}
}

func TestUnknownPrefix(t *testing.T) {
	fs := makeMemMapFs(t, map[string][]byte{
		"/strange/something": []byte(`github file`),
	})
	buf, err := dumpMemMapFsToBuf(fs)
	require.NoError(t, err)

	_, err = ReadArchive(buf)
	require.Error(t, err)
	require.Equal(t, err.Error(),
		"unknown file prefix `strange` for file `strange/something`")
}

func TestFilenamePwdResolve(t *testing.T) {
	var tests = []struct {
		Filename, Pwd, version              string
		expectedFilenameURL, expectedPwdURL *url.URL
		expectedError                       string
	}{
		{
			Filename:            "/home/nobody/something.js",
			Pwd:                 "/home/nobody",
			expectedFilenameURL: &url.URL{Scheme: "file", Path: "/home/nobody/something.js"},
			expectedPwdURL:      &url.URL{Scheme: "file", Path: "/home/nobody"},
		},
		{
			Filename:            "github.com/loadimpact/k6/samples/http2.js",
			Pwd:                 "github.com/loadimpact/k6/samples",
			expectedFilenameURL: &url.URL{Opaque: "github.com/loadimpact/k6/samples/http2.js"},
			expectedPwdURL:      &url.URL{Opaque: "github.com/loadimpact/k6/samples"},
		},
		{
			Filename:            "cdnjs.com/libraries/Faker",
			Pwd:                 "/home/nobody",
			expectedFilenameURL: &url.URL{Opaque: "cdnjs.com/libraries/Faker"},
			expectedPwdURL:      &url.URL{Scheme: "file", Path: "/home/nobody"},
		},
		{
			Filename:            "example.com/something/dot.js",
			Pwd:                 "example.com/something/",
			expectedFilenameURL: &url.URL{Host: "example.com", Scheme: "", Path: "/something/dot.js"},
			expectedPwdURL:      &url.URL{Host: "example.com", Scheme: "", Path: "/something"},
		},
		{
			Filename:            "https://example.com/something/dot.js",
			Pwd:                 "https://example.com/something",
			expectedFilenameURL: &url.URL{Host: "example.com", Scheme: "https", Path: "/something/dot.js"},
			expectedPwdURL:      &url.URL{Host: "example.com", Scheme: "https", Path: "/something"},
			version:             "0.25.0",
		},
		{
			Filename:      "ftps://example.com/something/dot.js",
			Pwd:           "https://example.com/something",
			expectedError: "only supported schemes for imports are file and https",
			version:       "0.25.0",
		},

		{
			Filename:      "https://example.com/something/dot.js",
			Pwd:           "ftps://example.com/something",
			expectedError: "only supported schemes for imports are file and https",
			version:       "0.25.0",
		},
	}

	for _, test := range tests {
		metadata := `{
		"filename": "` + test.Filename + `",
		"pwd": "` + test.Pwd + `",
		"k6version": "` + test.version + `"
	}`

		buf, err := dumpMemMapFsToBuf(makeMemMapFs(t, map[string][]byte{
			"/metadata.json": []byte(metadata),
		}))
		require.NoError(t, err)

		arc, err := ReadArchive(buf)
		if test.expectedError != "" {
			require.Error(t, err)
			require.Contains(t, err.Error(), test.expectedError)
		} else {
			require.NoError(t, err)
			require.Equal(t, test.expectedFilenameURL, arc.FilenameURL)
			require.Equal(t, test.expectedPwdURL, arc.PwdURL)
		}
	}
}
