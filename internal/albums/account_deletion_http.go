package albums

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Gateway deliberately leaves this nil until complete erasure is deployable.
// Acceptance revokes access; it never certifies physical erasure.
type accountDeletionHTTP struct {
	accounts *MySQLAccounts
	now      func() time.Time
}

func (h *uploadHTTP) RequestAlbumAccountDeletion(c *gin.Context) {
	if h.login == nil || h.login.AccountDeletion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "account_deletion_unavailable"})
		return
	}
	if !h.allowLogin(c, "account-deletion") {
		return
	}
	token, err := requestSessionToken(c.Request)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "account_authentication_required"})
		return
	}
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "json_required"})
		return
	}
	requestID, err := decodeAccountDeletion(c)
	if err != nil || c.Request.URL.RawQuery != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_account_deletion_request"})
		return
	}
	d := h.login.AccountDeletion
	receipt, err := d.accounts.requestAccountDeletion(c.Request.Context(), token, requestID, d.now)
	if errors.Is(err, ErrSession) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "account_authentication_required"})
		return
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "account_deletion_unavailable"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"requestID": receipt.RequestID, "requestedAt": receipt.RequestedAt, "state": "accepted"})
}

func decodeAccountDeletion(c *gin.Context) (string, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 512)
	d := json.NewDecoder(c.Request.Body)
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return "", ErrInvalidManifest
	}
	if token, err := d.Token(); err != nil || token != "requestID" {
		return "", ErrInvalidManifest
	}
	var requestID string
	if err := d.Decode(&requestID); err != nil || !validOwner(requestID) {
		return "", ErrInvalidManifest
	}
	// Exactly one field also rejects duplicate keys and caller-supplied owners.
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return "", ErrInvalidManifest
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return "", ErrInvalidManifest
	}
	return requestID, nil
}
