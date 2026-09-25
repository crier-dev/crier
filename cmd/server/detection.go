package main

// detection.go — the HTTP surface of the detection layer (CR-FEAT-030).
//
// Every route here is registered ONLY when CR_DETECT_ENABLED is on: the
// feature is additive, and a server without the flag must behave byte-for-byte
// as it did before this existed (the same posture CR_ENABLE_METRICS /
// CR_A2A_ENABLED established). None of them is auth-exempt — the kill-switch
// is the most powerful call on the bus and it demands the operator's Bearer
// token like every other authenticated route.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/detect"
)

// maxLogPage bounds one page of GET /delivery-log. The log itself is the file;
// the API serves a bounded page of the in-memory window so an operator (or a
// dashboard) cannot pull a hundred thousand records in one request by accident.
const maxLogPage = 1000

// maxKillSwitchReasonBytes bounds the operator's stated reason, which is
// recorded in the delivery log.
const maxKillSwitchReasonBytes = 512

// detectionHandlers serves the detection routes.
type detectionHandlers struct {
	det *detect.Detector
}

func writeDetectionJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// HandleDeliveryLog answers GET /delivery-log: the signed delivery records
// (who sent what to whom, when, and the verdict), plus the chain verification
// of the whole FILE and the honest window accounting the page came from.
//
// `limit` (default 100, max 1000) and `kind` (delivery|alert|containment) are
// HONORED or REFUSED, never ignored: a bad value is a 400 naming the bound.
func (h *detectionHandlers) HandleDeliveryLog(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxLogPage {
			writeDetectionJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("limit must be 1..%d", maxLogPage),
			})
			return
		}
		limit = n
	}
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "", detect.KindDelivery, detect.KindAlert, detect.KindContainment:
	default:
		writeDetectionJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("kind must be one of %s|%s|%s", detect.KindDelivery, detect.KindAlert, detect.KindContainment),
		})
		return
	}

	audit := h.det.Audit()
	if audit == nil {
		writeDetectionJSON(w, http.StatusOK, map[string]any{
			"enabled": true,
			"log":     nil,
			"detail":  "no CR_DETECT_LOG is configured: alerts and containment work, nothing is persisted",
			"records": []detect.Entry{},
		})
		return
	}
	writeDetectionJSON(w, http.StatusOK, map[string]any{
		"enabled":     true,
		"path":        audit.Path(),
		"total":       audit.Total(),
		"window":      len(audit.Entries(0, "")),
		"limit":       limit,
		"kind":        kind,
		"verify":      audit.Verify(),
		"records":     audit.Entries(limit, kind),
		"key_id":      audit.KeyID(),
		"append_only": "each record is signed with the server's ed25519 key and chained (prev_hash); a rewrite or deletion fails /delivery-log/verify",
	})
}

// HandleDeliveryLogVerify answers GET /delivery-log/verify: a fresh read of the
// FILE, re-verifying every record's signature, its position and the hash chain.
// This is the restart-survival check — the file is read from disk, so a record
// appended before a restart is verified the same way as one appended after it.
func (h *detectionHandlers) HandleDeliveryLogVerify(w http.ResponseWriter, r *http.Request) {
	audit := h.det.Audit()
	if audit == nil {
		writeDetectionJSON(w, http.StatusOK, map[string]any{
			"enabled": true,
			"log":     nil,
			"ok":      false,
			"detail":  "no CR_DETECT_LOG is configured, so there is no log to verify",
		})
		return
	}
	rep := audit.Verify()
	status := http.StatusOK
	if !rep.OK {
		// A log that does not verify is a live finding, not a 500: answer with
		// the finding and a status a dashboard cannot mistake for healthy.
		status = http.StatusConflict
	}
	writeDetectionJSON(w, status, rep)
}

// HandleAlerts answers GET /alerts: every baseline signal that has tripped,
// oldest first. `?signal=` filters by signal name and is validated, not
// ignored.
func (h *detectionHandlers) HandleAlerts(w http.ResponseWriter, r *http.Request) {
	signal := r.URL.Query().Get("signal")
	switch signal {
	case "", detect.SignalFanoutSpike, detect.SignalNewPeerBurst, detect.SignalOddHourVolume, detect.SignalCanaryTrip:
	default:
		writeDetectionJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown signal %q", signal),
		})
		return
	}
	alerts := h.det.Alerts()
	if signal != "" {
		filtered := make([]detect.Alert, 0, len(alerts))
		for _, a := range alerts {
			if a.Signal == signal {
				filtered = append(filtered, a)
			}
		}
		alerts = filtered
	}
	if alerts == nil {
		alerts = []detect.Alert{}
	}
	cfg := h.det.Config()
	writeDetectionJSON(w, http.StatusOK, map[string]any{
		"alerts": alerts,
		"count":  len(alerts),
		"thresholds": map[string]any{
			"fanout_spike":     map[string]any{"window_seconds": int(cfg.FanoutWindow.Seconds()), "distinct_targets": cfg.FanoutMinTargets},
			"new_peer_burst":   map[string]any{"window_seconds": int(cfg.NewPeerWindow.Seconds()), "new_peers": cfg.NewPeerMinTargets},
			"odd_hour_volume":  map[string]any{"quiet_hours_utc": fmt.Sprintf("%02d:00-%02d:00", cfg.QuietStartHour, cfg.QuietEndHour), "messages": cfg.QuietMinMessages},
			"canary_trip":      map[string]any{"tokens_touched": 1},
			"max_payload_scan": 65536,
		},
		"stats": h.det.Stats(),
	})
}

// HandleCanaries answers GET /canaries: the planted canary tokens, with the
// agent id that stands for each. It is authenticated like every other route —
// the tokens must be readable so an operator can plant them (a file, an env
// var, an agent's memory), and the route is the same trust level as the
// kill-switch.
func (h *detectionHandlers) HandleCanaries(w http.ResponseWriter, r *http.Request) {
	canaries := h.det.Canaries()
	writeDetectionJSON(w, http.StatusOK, map[string]any{
		"canaries": canaries,
		"count":    len(canaries),
		"detail":   "a delivery addressed to a canary id, or carrying a canary token in its payload, trips canary_trip (critical). Generated canaries rotate per boot; set CR_CANARY_TOKENS for stable ones.",
	})
}

// HandleKillSwitch answers POST /agents/{id}/kill-switch: the single containment
// call. Every step and its outcome is in the response, so a partial containment
// can never read as a clean one.
func (h *detectionHandlers) HandleKillSwitch(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var req struct {
		Reason string `json:"reason,omitempty"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxKillSwitchReasonBytes+1))
		if err != nil {
			writeDetectionJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
			return
		}
		if len(raw) > maxKillSwitchReasonBytes {
			writeDetectionJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("kill-switch body must be <= %d bytes", maxKillSwitchReasonBytes),
			})
			return
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				writeDetectionJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
		}
	}
	containment := h.det.Contain(id, req.Reason)
	writeDetectionJSON(w, http.StatusOK, containment)
}
