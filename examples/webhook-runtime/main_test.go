package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/dranet/pkg/apis"
)

func TestProvider(t *testing.T) {
	for _, test := range []struct {
		path   string
		body   string
		status int
	}{
		{"/GetProfileConfig", `{"config":{"profile":"example.com/tuning"}}`, http.StatusOK},
		{"/GetProfileConfig", `{"config":{"profile":"unknown"}}`, http.StatusNotFound},
		{"/GetProfileConfig", `{`, http.StatusBadRequest},
		{"/GetRuntimeHook", `{"config":{"profile":"example.com/tuning"}}`, http.StatusOK},
		{"/GetRuntimeHook", `{"config":{"profile":"unknown"}}`, http.StatusOK},
		{"/ReleaseProfileConfig", `{}`, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body)))
		if recorder.Code != test.status {
			t.Fatalf("%s: status %d, want %d", test.path, recorder.Code, test.status)
		}
		if test.path == "/GetRuntimeHook" && strings.Contains(test.body, tuningProfile) {
			var hook apis.RuntimeHook
			if err := json.Unmarshal(recorder.Body.Bytes(), &hook); err != nil {
				t.Fatal(err)
			}
			if err := hook.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestCNIWrapper(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the example wrapper")
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			directory := t.TempDir()
			plugin := filepath.Join(directory, "tuning")
			body := `#!/bin/sh
set -eu
[ "$CNI_COMMAND" = ADD ]
[ "$CNI_CONTAINERID" = pod-uid ]
[ "$CNI_NETNS" = /var/run/netns/example ]
[ "$CNI_IFNAME" = post0 ]
jq -e '.type == "tuning" and .sysctl["net.ipv4.conf.IFNAME.rp_filter"] == "0" and .prevResult.interfaces[0].name == "post0"' >/dev/null
`
			if fail {
				body += "printf '{\"code\":1,\"msg\":\"tuning rejected\"}'; exit 1\n"
			}
			if err := os.WriteFile(plugin, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("/bin/sh", "cni-tuning.sh", plugin)
			command.Env = []string{
				"DRANET_POD_UID=pod-uid", "DRANET_POD_NAMESPACE=default", "DRANET_POD_NAME=example",
				"DRANET_NETNS=/var/run/netns/example",
				`DRANET_CLAIMS=[{"devices":[{"config":{"interface":{"name":"post0"}},"data":{"delegate":"tuning","sysctl":{"net.ipv4.conf.IFNAME.rp_filter":"0"}}},{"data":{"delegate":"other"}}]}]`,
			}
			command.Stdin = strings.NewReader(`{"id":"container-id","status":"creating"}`)
			output, err := command.CombinedOutput()
			if (err != nil) != fail {
				t.Fatalf("wrapper output %q, error %v", output, err)
			}
			if fail && !strings.Contains(string(output), "tuning rejected") {
				t.Fatalf("missing plugin diagnostic in %q", output)
			}
		})
	}
}

func TestCNIWrapperDeviceSelection(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the example wrapper")
	}
	for _, test := range []struct {
		name   string
		claims string
		fail   bool
	}{
		{"unowned devices", `[{"devices":[{"data":{"delegate":"other"}}]}]`, false},
		{"no devices", `[]`, false},
		{"malformed data", `{`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("/bin/sh", "cni-tuning.sh", "/missing/plugin")
			command.Env = []string{"DRANET_POD_UID=pod-uid", "DRANET_NETNS=/var/run/netns/example", "DRANET_CLAIMS=" + test.claims}
			output, err := command.CombinedOutput()
			if (err != nil) != test.fail {
				t.Fatalf("output %q, error %v", output, err)
			}
		})
	}
}
