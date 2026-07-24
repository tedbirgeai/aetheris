package tunnel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier"
	"github.com/tedbirgeai/aetheris/internal/dedup"
	"github.com/tedbirgeai/aetheris/internal/middleware"
	"github.com/tedbirgeai/aetheris/internal/store"
)

// ChunkedRequest, parcali (tekillestirilmis) tunel istegidir.
type ChunkedRequest struct {
	CarrierType string          `json:"carrier_type"`
	Destination string          `json:"destination,omitempty"`
	Manifest    *dedup.Manifest `json:"manifest"`
	// Chunks, YALNIZCA Cached=false olan parcalarin base64 kodlu
	// sifreli halleri, manifest sirasiyla.
	Chunks []string `json:"chunks"`
}

// ChunkedReceipt, parcali gecis makbuzudur.
type ChunkedReceipt struct {
	Status       string `json:"status"`
	Protocol     string `json:"protocol"`
	ClientID     string `json:"client_id"`
	CarrierUsed  string `json:"carrier_used"`
	MeteredBytes uint64 `json:"metered_bytes"`
	ManifestSHA  string `json:"manifest_sha256"`
	Timestamp    int64  `json:"timestamp"`

	// Verification, sunucunun NEYI dogruladigini DURUSTCE bildirir.
	Verification *dedup.VerificationResult `json:"verification"`

	// SavingsClaimed, istemcinin onbellekten karsiladigini BEYAN ettigi
	// bayt. Alan adi bilincli olarak "claimed" icerir: sunucu bu beyani
	// dogrulayamaz, yalnizca raporlar.
	SavingsClaimedBytes uint64 `json:"savings_claimed_bytes"`

	Signature string `json:"signature"`
}

// TunnelChunked, POST /api/v1/tunnel/chunked
//
// # FATURALAMA KURALI
//
// Faturalama, istemcinin tasarruf IDDIASINA gore degil, TELDE GERCEKTEN
// AKAN bayta gore yapilir. Istemci "bu parcayi onbellekten karsiladim"
// derse ve yalan soyluyorsa, veri hedefte eksik olur — kendi zarari.
// Sunucunun gelirine zarar veremez.
func (h *Handler) TunnelChunked(w http.ResponseWriter, r *http.Request) {
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

	var req ChunkedRequest
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

	// --- DURUST MANIFEST DOGRULAMASI ---
	// Sunucu yapisal butunlugu dogrular. Parca ozetlerinin gercekten
	// o duz metnin ozeti olup olmadigini DOGRULAYAMAZ (duz metni gormez)
	// ve dogruladigini iddia etmez.
	verification, err := dedup.VerifyManifest(req.Manifest, req.Chunks)
	if err != nil {
		h.writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Manifestin kendisinin ozeti — denetim izi icin.
	manBytes, err := json.Marshal(req.Manifest)
	if err != nil {
		h.writeErr(w, http.StatusInternalServerError, "manifest islenemedi")
		return
	}
	manSum := sha256.Sum256(manBytes)
	manSHA := hex.EncodeToString(manSum[:])

	// FATURALANAN: telde gercekten akan sifreli bayt.
	bytesIn := verification.TransferredBytes

	receipt := ChunkedReceipt{
		Status:              "metered",
		Protocol:            "Aetheris/1.2",
		ClientID:            clientID,
		CarrierUsed:         string(carrierType),
		MeteredBytes:        bytesIn,
		ManifestSHA:         manSHA,
		Timestamp:           time.Now().UTC().Unix(),
		Verification:        verification,
		SavingsClaimedBytes: verification.ReferencedBytes,
	}
	receipt.Signature = h.signChunked(receipt)

	body, err := json.Marshal(receipt)
	if err != nil {
		h.Logger.Error("makbuz kodlanamadi", "err", err)
		h.writeErr(w, http.StatusInternalServerError, "makbuz uretilemedi")
		return
	}

	usage := store.Usage{
		ClientID:    clientID,
		CarrierType: string(carrierType),
		BytesIn:     bytesIn,
		BytesOut:    uint64(len(body)),
		PayloadSHA:  manSHA,
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

	// Faturalama koprusune bildir (asenkron, bloklamaz).
	h.publishReceipt(clientID, string(carrierType), req.Destination,
		bytesIn, uint64(len(body)), manSHA, receipt.Signature)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		h.Logger.Warn("yanit yazilamadi", "client_id", clientID, "err", err)
	}
}

func (h *Handler) signChunked(rc ChunkedReceipt) string {
	rc.Signature = ""
	payload, err := json.Marshal(rc)
	if err != nil {
		return ""
	}
	return hmacHex(h.ReceiptSecret, payload)
}
