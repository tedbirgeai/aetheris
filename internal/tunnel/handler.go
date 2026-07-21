// Package tunnel - HTTP uclari.
//
// SIFIR BILGI SOZLESMESI: Bu sunucu tasidigi veriyi cozebilecek hicbir
// anahtara sahip DEGILDIR. Istemci veriyi kendi tarafinda AES-256-GCM ile
// sifreler; sunucu yalnizca boyutu olcer ve imzali makbuz doner.
// Sunucu tarafinda sifre cozme kodu bilincli olarak YOKTUR.
package tunnel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tedbirge-labs/aetheris-gateway/internal/carrier"
	"github.com/tedbirge-labs/aetheris-gateway/internal/meter"
	"github.com/tedbirge-labs/aetheris-gateway/internal/middleware"
)

type Handler struct {
	Meter           *meter.Meter
	Logger          *slog.Logger
	MaxPayloadBytes int64
	ReceiptSecret   []byte
}

type TunnelRequest struct {
	CarrierType string `json:"carrier_type"`
	Ciphertext  string `json:"ciphertext"`
	Destination string `json:"destination,omitempty"`
}

type Receipt struct {
	Status       string `json:"status"`
	Protocol     string `json:"protocol"`
	ClientID     string `json:"client_id"`
	CarrierUsed  string `json:"carrier_used"`
	MeteredBytes uint64 `json:"metered_bytes"`
	PayloadSHA   string `json:"payload_sha256"`
	Timestamp    int64  `json:"timestamp"`
	Signature    string `json:"signature"`
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.Logger.Error("yanit kodlanamadi", "err", err)
	}
}

func (h *Handler) writeErr(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, map[string]any{"status": "error", "error": msg})
}

func (h *Handler) Tunnel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeErr(w, http.StatusMethodNotAllowed, "yalnizca POST kabul edilir")
		return
	}

	clientID, ok := middleware.ClientIDFrom(r.Context())
	if !ok {
		h.writeErr(w, http.StatusUnauthorized, "kimlik dogrulanamadi")
		return
	}

	h.Meter.TunnelOpened()
	defer h.Meter.TunnelClosed()

	limited := http.MaxBytesReader(w, r.Body, h.MaxPayloadBytes)
	defer r.Body.Close()

	var req TunnelRequest
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.writeErr(w, http.StatusRequestEntityTooLarge, "yuk azami boyutu asiyor")
			return
		}
		h.writeErr(w, http.StatusBadRequest, "gecersiz JSON govdesi")
		return
	}

	carrierType, err := carrier.Normalize(req.CarrierType)
	if err != nil {
		h.writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if req.Ciphertext == "" {
		h.writeErr(w, http.StatusBadRequest, "ciphertext alani zorunludur")
		return
	}

	opaque, err := base64.StdEncoding.DecodeString(req.Ciphertext)
	if err != nil {
		h.writeErr(w, http.StatusBadRequest, "ciphertext gecerli base64 degil")
		return
	}

	if len(opaque) < 28 {
		h.writeErr(w, http.StatusBadRequest, "ciphertext AES-GCM icin fazla kisa (asgari 28 bayt)")
		return
	}

	bytesIn := uint64(len(opaque))
	sum := sha256.Sum256(opaque)

	receipt := Receipt{
		Status:       "routed",
		Protocol:     "Aetheris/1.0",
		ClientID:     clientID,
		CarrierUsed:  string(carrierType),
		MeteredBytes: bytesIn,
		PayloadSHA:   hex.EncodeToString(sum[:]),
		Timestamp:    time.Now().UTC().Unix(),
	}
	receipt.Signature = h.sign(receipt)

	body, err := json.Marshal(receipt)
	if err != nil {
		h.Logger.Error("makbuz kodlanamadi", "err", err)
		h.writeErr(w, http.StatusInternalServerError, "makbuz uretilemedi")
		return
	}

	h.Meter.Record(clientID, string(carrierType), bytesIn, uint64(len(body)))

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		h.Logger.Warn("yanit yazilamadi", "client_id", clientID, "err", err)
	}
}

func (h *Handler) sign(rc Receipt) string {
	rc.Signature = ""
	payload, err := json.Marshal(rc)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, h.ReceiptSecret)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) MyUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		h.writeErr(w, http.StatusMethodNotAllowed, "yalnizca GET kabul edilir")
		return
	}
	clientID, ok := middleware.ClientIDFrom(r.Context())
	if !ok {
		h.writeErr(w, http.StatusUnauthorized, "kimlik dogrulanamadi")
		return
	}
	entry, found := h.Meter.ClientSnapshot(clientID)
	if !found {
		h.writeJSON(w, http.StatusOK, map[string]any{
			"client_id": clientID,
			"usage":     nil,
			"message":   "bu istemci icin henuz kayitli trafik yok",
		})
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"client_id": clientID,
		"usage":     entry,
	})
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"protocol":           "Aetheris/1.0",
		"supported_carriers": carrier.All(),
	})
}
