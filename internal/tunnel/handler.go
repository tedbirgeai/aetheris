// Package tunnel - HTTP uclari.
//
// SIFIR BILGI SOZLESMESI: Bu sunucu tasidigi veriyi cozebilecek hicbir
// anahtara sahip DEGILDIR. Istemci veriyi kendi tarafinda AES-256-GCM ile
// sifreler; sunucu yalnizca boyutu olcer, opak blogu iletir ve imzali
// makbuz doner. Sunucu tarafinda sifre cozme kodu bilincli olarak YOKTUR.
// Bu, "veriyi okumuyoruz" iddiasini bir taahhut degil, mimari bir
// imkansizlik haline getirir.
package tunnel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tedbirgeai/aetheris/internal/billing"
	"github.com/tedbirgeai/aetheris/internal/carrier"
	"github.com/tedbirgeai/aetheris/internal/meter"
	"github.com/tedbirgeai/aetheris/internal/middleware"
	"github.com/tedbirgeai/aetheris/internal/router"
	"github.com/tedbirgeai/aetheris/internal/store"
)

type Handler struct {
	Meter           *meter.Meter
	Router          *router.Router
	Prober          *router.HealthProber
	Logger          *slog.Logger
	MaxPayloadBytes int64
	ReceiptSecret   []byte

	// --- v0.3b ---
	// Billing, fatura olaylarini dis sistemlere ileten kopru (nil olabilir).
	Billing *billing.Bridge
	// Credits, role teskik motoru (nil olabilir).
	Credits *billing.CreditEngine
	// Thresholds, kullanim esigi izleyici (nil olabilir).
	Thresholds *billing.ThresholdWatcher
	// TLSStatus, health ucunda sertifika durumunu bildirmek icin (nil olabilir).
	TLSStatus func() any
}

// hmacHex, HMAC-SHA256 imzasini hex olarak dondurur.
func hmacHex(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// publishReceipt, basarili bir olcumu faturalama koprusune bildirir.
// ASENKRONDUR; istegi bloklamaz.
func (h *Handler) publishReceipt(clientID, carrierType, destination string,
	bytesIn, bytesOut uint64, payloadSHA, signature string) {

	if h.Billing == nil {
		return
	}
	// Olay kimligi: ayni makbuzun iki kez faturalanmamasi icin
	// imzadan turetilir (imza makbuza ozgudur).
	id := signature
	if len(id) > 32 {
		id = id[:32]
	}
	h.Billing.Publish(billing.Event{
		ID:          "receipt-" + id,
		Type:        billing.ReceiptGenerated,
		ClientID:    clientID,
		CarrierType: carrierType,
		Destination: destination,
		BytesIn:     bytesIn,
		BytesOut:    bytesOut,
		PayloadSHA:  payloadSHA,
		Signature:   signature,
		OccurredAt:  time.Now().UTC(),
	})

	// Kullanim esigi kontrolu.
	if h.Thresholds != nil {
		if e, err := h.Meter.ClientUsage(context.Background(), clientID); err == nil {
			h.Thresholds.Check(clientID, e.BytesIn+e.BytesOut)
		}
	}
}

type TunnelRequest struct {
	CarrierType string `json:"carrier_type"`
	Ciphertext  string `json:"ciphertext"`
	Destination string `json:"destination,omitempty"`
}

type Receipt struct {
	Status       string         `json:"status"`
	Protocol     string         `json:"protocol"`
	ClientID     string         `json:"client_id"`
	CarrierUsed  string         `json:"carrier_used"`
	MeteredBytes uint64         `json:"metered_bytes"`
	PayloadSHA   string         `json:"payload_sha256"`
	Timestamp    int64          `json:"timestamp"`
	Route        *router.Result `json:"route,omitempty"`
	Signature    string         `json:"signature"`
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

// Tunnel, POST /api/v1/tunnel
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

	// Base64 cozumu YALNIZCA boyut olcmek ve butunluk ozeti almak icindir.
	// Elde edilen baytlar sifreli metindir; sunucunun cozme anahtari yoktur.
	opaque, err := base64.StdEncoding.DecodeString(req.Ciphertext)
	if err != nil {
		h.writeErr(w, http.StatusBadRequest, "ciphertext gecerli base64 degil")
		return
	}

	// AES-GCM ciktisi en az nonce(12) + tag(16) = 28 bayttir.
	if len(opaque) < 28 {
		h.writeErr(w, http.StatusBadRequest,
			"ciphertext AES-GCM icin fazla kisa (asgari 28 bayt: 12 nonce + 16 tag)")
		return
	}

	bytesIn := uint64(len(opaque))
	sum := sha256.Sum256(opaque)
	shaHex := hex.EncodeToString(sum[:])

	// --- Yonlendirme ---
	var routeResult *router.Result
	if req.Destination != "" {
		if h.Router == nil || !h.Router.Enabled() {
			h.writeErr(w, http.StatusServiceUnavailable,
				"yonlendirme devre disi: destination belirtildi ancak rota tanimli degil")
			return
		}
		// Failover prober aktifse hedefi cozup (gerekirse yedege dusurup)
		// oyle yonlendir. Degilse dogrudan Forward.
		if h.Prober != nil {
			var failedOver bool
			routeResult, failedOver, err = h.Prober.ForwardWithFailover(
				r.Context(), req.Destination, clientID, carrierType, opaque)
			if failedOver && err == nil {
				h.Logger.Info("istek failover ile yonlendirildi",
					"client_id", clientID, "requested", req.Destination,
					"actual", routeResult.RouteName)
			}
		} else {
			routeResult, err = h.Router.Forward(r.Context(), req.Destination, clientID, carrierType, opaque)
		}
		if err != nil {
			switch {
			case errors.Is(err, router.ErrNoRoute):
				h.writeErr(w, http.StatusUnprocessableEntity, err.Error())
			case errors.Is(err, router.ErrCarrierNotAllowed):
				h.writeErr(w, http.StatusUnprocessableEntity, err.Error())
			default:
				h.Logger.Error("yonlendirme basarisiz",
					"client_id", clientID, "destination", req.Destination, "err", err)
				h.writeErr(w, http.StatusBadGateway, "upstream'e iletim basarisiz")
			}
			return
		}
	}

	status := "metered"
	if routeResult != nil {
		status = "routed"
	}

	receipt := Receipt{
		Status:       status,
		Protocol:     "Aetheris/1.1",
		ClientID:     clientID,
		CarrierUsed:  string(carrierType),
		MeteredBytes: bytesIn,
		PayloadSHA:   shaHex,
		Timestamp:    time.Now().UTC().Unix(),
		Route:        routeResult,
	}
	receipt.Signature = h.sign(receipt)

	body, err := json.Marshal(receipt)
	if err != nil {
		h.Logger.Error("makbuz kodlanamadi", "err", err)
		h.writeErr(w, http.StatusInternalServerError, "makbuz uretilemedi")
		return
	}

	// Giden bayt: makbuz + (yonlendirme yapildiysa) upstream'e giden yuk.
	bytesOut := uint64(len(body))
	if routeResult != nil {
		bytesOut += routeResult.BytesSent
	}

	// FAIL-CLOSED: Defter yazilamiyorsa hizmet verilmez.
	// Olculemeyen trafik faturalanamaz; sessizce gecirmek gelir kaybidir.
	usage := store.Usage{
		ClientID:    clientID,
		CarrierType: string(carrierType),
		BytesIn:     bytesIn,
		BytesOut:    bytesOut,
		PayloadSHA:  shaHex,
		Signature:   receipt.Signature,
		Destination: req.Destination,
		OccurredAt:  time.Now().UTC(),
	}
	if err := h.Meter.Record(r.Context(), usage); err != nil {
		h.Logger.Error("defter yazilamadi - istek reddedildi",
			"client_id", clientID, "err", err)
		h.writeErr(w, http.StatusServiceUnavailable,
			"olcum defteri kullanilamiyor, istek islenmedi")
		return
	}

	// Faturalama koprusune bildir (asenkron, istegi bloklamaz).
	h.publishReceipt(clientID, string(carrierType), req.Destination,
		bytesIn, bytesOut, shaHex, receipt.Signature)

	// Role kredisi: istek baska bir dugume yonlendirildiyse, yonlendiren
	// musteri kredi kazanir. Kendi trafigini role etmek kredi kazandirmaz
	// (CreditEngine icinde engellenir).
	if h.Credits != nil && routeResult != nil {
		h.Credits.RecordRelay(clientID, req.Destination, routeResult.BytesSent)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		h.Logger.Warn("yanit yazilamadi", "client_id", clientID, "err", err)
	}
}

// sign, makbuzu HMAC-SHA256 ile imzalar (Signature alani haric).
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

// MyUsage, GET /api/v1/meter/me
// Istemci YALNIZCA kendi defterini gorur. Baska istemcilerin tuketimi sizmaz.
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

	entry, err := h.Meter.ClientUsage(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		h.writeJSON(w, http.StatusOK, map[string]any{
			"client_id": clientID,
			"usage":     nil,
			"message":   "bu istemci icin henuz kayitli trafik yok",
		})
		return
	}
	if err != nil {
		h.Logger.Error("defter okunamadi", "client_id", clientID, "err", err)
		h.writeErr(w, http.StatusServiceUnavailable, "olcum defteri kullanilamiyor")
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]any{
		"client_id": clientID,
		"usage":     entry,
	})
}

// Health, GET /healthz - kimlik dogrulamasi gerektirmez.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	routes := map[string]string{}
	if h.Router != nil {
		routes = h.Router.Routes()
	}
	body := map[string]any{
		"status":             "ok",
		"protocol":           "Aetheris/1.1",
		"store":              h.Meter.Kind(),
		"active_tunnels":     h.Meter.ActiveTunnels(),
		"uptime_seconds":     h.Meter.UptimeSeconds(),
		"supported_carriers": carrier.All(),
		"routes":             routes,
	}
	if h.Prober != nil {
		body["route_health"] = h.Prober.Status()
		body["qos"] = h.Prober.QoS()
	}
	if h.Billing != nil {
		body["billing"] = h.Billing.Stats()
	}
	if h.TLSStatus != nil {
		body["tls"] = h.TLSStatus()
	}
	h.writeJSON(w, http.StatusOK, body)
}
