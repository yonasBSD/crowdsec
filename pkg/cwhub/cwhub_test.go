package cwhub

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crowdsecurity/crowdsec/pkg/csconfig"
)

const mockURLTemplate = "https://cdn-hub.crowdsec.net/crowdsecurity/%s/%s"

/*
 To test :
  - Download 'first' hub index
  - Update hub index
  - Install collection + list content
  - Taint existing parser + list
  - Upgrade collection
*/

var responseByPath map[string]string

// testHubOld initializes a temporary hub with an empty json file, optionally updating it.
func testHubOld(t *testing.T, update bool) *Hub {
	ctx := t.Context()
	tmpDir := t.TempDir()

	local := &csconfig.LocalHubCfg{
		HubDir:         filepath.Join(tmpDir, "crowdsec", "hub"),
		HubIndexFile:   filepath.Join(tmpDir, "crowdsec", "hub", ".index.json"),
		InstallDir:     filepath.Join(tmpDir, "crowdsec"),
		InstallDataDir: filepath.Join(tmpDir, "installed-data"),
	}

	err := os.MkdirAll(local.HubDir, 0o700)
	require.NoError(t, err)

	err = os.MkdirAll(local.InstallDir, 0o700)
	require.NoError(t, err)

	err = os.MkdirAll(local.InstallDataDir, 0o700)
	require.NoError(t, err)

	err = os.WriteFile(local.HubIndexFile, []byte("{}"), 0o644)
	require.NoError(t, err)

	hub, err := NewHub(local, log.StandardLogger())
	require.NoError(t, err)

	if update {
		indexProvider := &Downloader{
			Branch:      "master",
			URLTemplate: mockURLTemplate,
		}

		updated, err := hub.Update(ctx, indexProvider, false)
		require.NoError(t, err)
		assert.True(t, updated)
	}

	err = hub.Load()
	require.NoError(t, err)

	return hub
}

// envSetup initializes the temporary hub and mocks the http client.
func envSetup(t *testing.T) *Hub {
	setResponseByPath(t)
	log.SetLevel(log.DebugLevel)

	defaultTransport := HubClient.Transport

	t.Cleanup(func() {
		HubClient.Transport = defaultTransport
	})

	// Mock the http client
	HubClient.Transport = newMockTransport()

	hub := testHubOld(t, true)

	return hub
}

type mockTransport struct{}

func newMockTransport() http.RoundTripper {
	return &mockTransport{}
}

// Implement http.RoundTripper.
func (t *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Create mocked http.Response
	response := &http.Response{
		Header:     make(http.Header),
		Request:    req,
		StatusCode: http.StatusOK,
	}
	response.Header.Set("Content-Type", "application/json")

	log.Infof("---> %s", req.URL.Path)

	// FAKE PARSER
	resp, ok := responseByPath[req.URL.Path]
	if !ok {
		return nil, fmt.Errorf("unexpected url: %s", req.URL.Path)
	}

	response.Body = io.NopCloser(strings.NewReader(resp))

	return response, nil
}

func fileToStringX(t *testing.T, path string) string {
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	data, err := io.ReadAll(f)
	require.NoError(t, err)

	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func setResponseByPath(t *testing.T) {
	responseByPath = map[string]string{
		"/crowdsecurity/master/parsers/s01-parse/crowdsecurity/foobar_parser.yaml":    fileToStringX(t, "./testdata/foobar_parser.yaml"),
		"/crowdsecurity/master/parsers/s01-parse/crowdsecurity/foobar_subparser.yaml": fileToStringX(t, "./testdata/foobar_parser.yaml"),
		"/crowdsecurity/master/collections/crowdsecurity/test_collection.yaml":        fileToStringX(t, "./testdata/collection_v1.yaml"),
		"/crowdsecurity/master/.index.json":                                           fileToStringX(t, "./testdata/index1.json"),
		"/crowdsecurity/master/scenarios/crowdsecurity/foobar_scenario.yaml": `filter: true
name: crowdsecurity/foobar_scenario`,
		"/crowdsecurity/master/scenarios/crowdsecurity/barfoo_scenario.yaml": `filter: true
name: crowdsecurity/foobar_scenario`,
		"/crowdsecurity/master/collections/crowdsecurity/foobar_subcollection.yaml": `
blah: blalala
qwe: jejwejejw`,
		"/crowdsecurity/master/collections/crowdsecurity/foobar.yaml": `
blah: blalala
qwe: jejwejejw`,
	}
}
