package gossip

import (
	"crypto/sha256"
	"encoding/hex"
)

// NewRecord, veriden icerik-adresli bir Record uretir. ID, verinin
// SHA-256 ozetidir: ayni veri her dugumde AYNI ID'yi verir. Bu, gossip
// yayilirken ayni kaydin farkli dugumlerde tekrar tekrar cogaltilmamasini
// (idempotency) ucretsiz saglar ve iki tarafin kayitlarini birlestirmeyi
// deterministik kilar.
func NewRecord(data []byte) Record {
	sum := sha256.Sum256(data)
	cp := make([]byte, len(data))
	copy(cp, data)
	return Record{ID: hex.EncodeToString(sum[:]), Data: cp}
}
