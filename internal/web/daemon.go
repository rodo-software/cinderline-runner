package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/rodo-software/cinderline-runner/internal/localapi"
	"github.com/rodo-software/cinderline-runner/internal/manage"
)

// updating is the page's update: "running", "failed: <text>" or "";
// package-wide, as a moved page gets a new Handler.
var updating struct {
	sync.Mutex
	state string
}

// statusJSON is the daemon's status as the page gets it, with the update's.
type statusJSON struct {
	localapi.Status
	Update string `json:"update,omitempty"`
}

func (h *Handlers) status() statusJSON {
	updating.Lock()
	defer updating.Unlock()
	return statusJSON{h.Status(), updating.state}
}

// daemonRoutes adds the daemon's actions to mux: stop, drain, update and
// rotating the CA, each a POST without a body (CLR-303).
func daemonRoutes(mux *http.ServeMux, h *Handlers) {
	mux.HandleFunc("POST /api/daemon/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		http.NewResponseController(w).Flush() // answer before the server closes
		h.Stop()
	})
	mux.HandleFunc("POST /api/daemon/drain", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		http.NewResponseController(w).Flush()
		h.Drain()
	})
	// No reload: each container takes the CA as it is when it starts.
	mux.HandleFunc("POST /api/daemon/rotate-ca", func(w http.ResponseWriter, r *http.Request) {
		a, err := manage.RotateCA(h.Paths)
		if err != nil {
			writeError(w, err)
			return
		}
		h.Log.Info("runner CA rotated", "certificate", a.CertFile, "expires", a.Cert.NotAfter)
		w.WriteHeader(http.StatusNoContent)
	})
	// The same script from the same place as `update`; nothing of the request is used.
	mux.HandleFunc("POST /api/daemon/update", func(w http.ResponseWriter, r *http.Request) {
		updating.Lock()
		running := updating.state == "running"
		if !running {
			updating.state = "running"
		}
		updating.Unlock()
		if running {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "an update runs already"})
			return
		}
		go h.update()
		w.WriteHeader(http.StatusAccepted)
	})
}

// update installs the latest release and, when it is new, has the daemon
// restart with it once its runs have finished; the outcome goes to the log
// and to updating. After a restart is asked for, updating stays "running"
// until the new binary runs.
func (h *Handlers) update() {
	h.Log.Info("update started")
	exe, err := manage.Exe()
	v := ""
	if err == nil {
		// shortcut: not cancelled when the daemon stops; the process ends it, the install script may run on to its end.
		v, err = manage.Update(context.Background(), h.Exec, exe, h.Version)
	}
	state := ""
	switch {
	case err != nil:
		h.Log.Error("update failed", "err", err)
		state = "failed: " + err.Error()
	case v == h.Version:
		h.Log.Info("update: already at the latest release", "version", v)
	default:
		h.Log.Info("updated; restarting with the new binary once the runs have finished", "from", h.Version, "to", v)
		state = "running"
	}
	updating.Lock()
	updating.state = state
	updating.Unlock()
	if state == "running" {
		h.Restart()
	}
}
