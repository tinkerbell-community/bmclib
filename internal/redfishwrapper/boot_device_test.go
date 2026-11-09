package redfishwrapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBootBMC serves the Boot object of /redfish/v1/Systems/1 the way bmcweb
// does on an ASRock Rack X570D4I-2T: it reports read-only annotations and
// TrustedModuleRequiredToBoot as a string, and rejects a PATCH carrying either
// with 400, applying nothing.
type fakeBootBMC struct {
	enabled, target, mode string
	// modes are the boot modes the BMC accepts; a PATCH setting another fails.
	modes []string
	// noMode makes the BMC report no BootSourceOverrideMode at all.
	noMode bool
	// patches records the Boot object of every PATCH, accepted or not.
	patches []map[string]any
}

func (f *fakeBootBMC) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			boot := map[string]any{
				"AutomaticRetryConfig":                             "RetryAttempts",
				"BootSourceOverrideEnabled":                        f.enabled,
				"BootSourceOverrideTarget":                         f.target,
				"BootSourceOverrideTarget@Redfish.AllowableValues": []string{"None", "Pxe", "Hdd", "Cd", "Diags", "BiosSetup", "Usb"},
				"StopBootOnFault":                                  "Never",
				"TrustedModuleRequiredToBoot":                      "Disabled",
			}
			if !f.noMode {
				boot["BootSourceOverrideMode"] = f.mode
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"@odata.id": "/redfish/v1/Systems/1",
				"Id":        "1",
				"Boot":      boot,
			})
		case http.MethodPatch:
			var body struct{ Boot map[string]any }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.patches = append(f.patches, body.Boot)
			for k, v := range body.Boot {
				switch k {
				case "BootSourceOverrideEnabled", "BootSourceOverrideTarget", "AutomaticRetryConfig", "StopBootOnFault":
				case "BootSourceOverrideMode":
					if f.noMode || !slices.Contains(f.modes, v.(string)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				default: // annotations, TrustedModuleRequiredToBoot
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			if v, ok := body.Boot["BootSourceOverrideEnabled"].(string); ok {
				f.enabled = v
			}
			if v, ok := body.Boot["BootSourceOverrideTarget"].(string); ok {
				f.target = v
			}
			if v, ok := body.Boot["BootSourceOverrideMode"].(string); ok {
				f.mode = v
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func newBootTestClient(t *testing.T, bmc *fakeBootBMC) *Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/redfish/v1/", endpointFunc(t, "serviceroot.json"))
	mux.HandleFunc("/redfish/v1/Systems", endpointFunc(t, "systems.json"))
	mux.HandleFunc("/redfish/v1/Systems/1", bmc.handler(t))

	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	parsedURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	client := NewClient(parsedURL.Hostname(), parsedURL.Port(), "", "", WithBasicAuthEnabled(true))
	ctx := context.Background()
	require.NoError(t, client.Open(ctx))
	t.Cleanup(func() { _ = client.Close(ctx) })

	return client
}

func TestSystemBootDeviceSet(t *testing.T) {
	tests := map[string]struct {
		bmc        fakeBootBMC
		persistent bool
		efiBoot    bool
		wantOK     bool
		wantErr    string
		// want is the BMC's boot override afterwards: enabled, target, mode.
		want [3]string
		// wantKeys are the Boot properties of each PATCH sent.
		wantKeys [][]string
	}{
		"UEFI once, sending only the override": {
			bmc:      fakeBootBMC{enabled: "Disabled", target: "None", mode: "Legacy", modes: []string{"Legacy", "UEFI"}},
			efiBoot:  true,
			wantOK:   true,
			want:     [3]string{"Once", "Pxe", "UEFI"},
			wantKeys: [][]string{{"BootSourceOverrideEnabled", "BootSourceOverrideMode", "BootSourceOverrideTarget"}},
		},
		"Legacy continuous": {
			bmc:        fakeBootBMC{enabled: "Disabled", target: "None", mode: "UEFI", modes: []string{"Legacy", "UEFI"}},
			persistent: true,
			wantOK:     true,
			want:       [3]string{"Continuous", "Pxe", "Legacy"},
			wantKeys:   [][]string{{"BootSourceOverrideEnabled", "BootSourceOverrideMode", "BootSourceOverrideTarget"}},
		},
		"a UEFI-only BMC rejecting Legacy still sets the device": {
			bmc:    fakeBootBMC{enabled: "Disabled", target: "None", mode: "UEFI", modes: []string{"UEFI"}},
			wantOK: true,
			want:   [3]string{"Once", "Pxe", "UEFI"},
			wantKeys: [][]string{
				{"BootSourceOverrideEnabled", "BootSourceOverrideMode", "BootSourceOverrideTarget"},
				{"BootSourceOverrideEnabled", "BootSourceOverrideTarget"},
			},
		},
		"a BMC rejecting UEFI fails instead of booting Legacy": {
			bmc:     fakeBootBMC{enabled: "Disabled", target: "None", mode: "Legacy", modes: []string{"Legacy"}},
			efiBoot: true,
			wantErr: "the BMC refused boot mode UEFI and kept Legacy",
			want:    [3]string{"Once", "Pxe", "Legacy"},
			wantKeys: [][]string{
				{"BootSourceOverrideEnabled", "BootSourceOverrideMode", "BootSourceOverrideTarget"},
				{"BootSourceOverrideEnabled", "BootSourceOverrideTarget"},
			},
		},
		"a BMC without boot modes gets none": {
			bmc:      fakeBootBMC{enabled: "Disabled", target: "None", noMode: true},
			efiBoot:  true,
			wantOK:   true,
			want:     [3]string{"Once", "Pxe", ""},
			wantKeys: [][]string{{"BootSourceOverrideEnabled", "BootSourceOverrideTarget"}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bmc := tt.bmc
			client := newBootTestClient(t, &bmc)

			ok, err := client.SystemBootDeviceSet(context.Background(), "pxe", tt.persistent, tt.efiBoot)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, [3]string{bmc.enabled, bmc.target, bmc.mode})

			var gotKeys [][]string
			for _, p := range bmc.patches {
				keys := make([]string, 0, len(p))
				for k := range p {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				gotKeys = append(gotKeys, keys)
			}
			assert.Equal(t, tt.wantKeys, gotKeys)
		})
	}
}
