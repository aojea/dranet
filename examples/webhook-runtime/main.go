package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider/webhook"
)

const tuningProfile = "example.com/tuning"

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, webhook.Capabilities{ProfileProvider: true, RuntimeHook: true})
	})
	mux.HandleFunc("POST /GetProfileConfig", func(w http.ResponseWriter, r *http.Request) {
		var req webhook.ProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Config == nil || req.Config.Profile != tuningProfile {
			http.Error(w, "unknown profile", http.StatusNotFound)
			return
		}
		writeJSON(w, apis.NetworkConfig{})
	})
	mux.HandleFunc("POST /GetRuntimeHook", func(w http.ResponseWriter, r *http.Request) {
		var req webhook.ProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Config == nil || req.Config.Profile != tuningProfile {
			writeJSON(w, nil)
			return
		}
		writeJSON(w, apis.RuntimeHook{
			Path:           "/opt/dranet/bin/cni-tuning.sh",
			Args:           []string{"/opt/cni/bin/tuning"},
			TimeoutSeconds: 10,
			Data:           json.RawMessage(`{"delegate":"tuning","sysctl":{"net.ipv4.conf.IFNAME.rp_filter":"0"}}`),
		})
	})
	mux.HandleFunc("POST /ReleaseProfileConfig", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, struct{}{})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func main() {
	address := flag.String("bind-address", "127.0.0.1:8082", "webhook listen address")
	flag.Parse()
	server := &http.Server{Addr: *address, Handler: handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
