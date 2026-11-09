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
	token, requestID, ok := h.accountDeletionInput(c)
	if !ok {
		return
	}
	d := h.login.AccountDeletion
	receipt, err := d.accounts.requestAccountDeletion(c.Request.Context(), token, requestID, d.now)
	if writeAccountDeletionError(c, err) {
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"requestID": receipt.RequestID, "requestedAt": receipt.RequestedAt, "state": "accepted"})
}

func (h *uploadHTTP) WithdrawAlbumAccountDeletion(c *gin.Context) {
	token, requestID, ok := h.accountDeletionInput(c)
	if !ok {
		return
	}
	d := h.login.AccountDeletion
	receipt, err := d.accounts.withdrawAccountDeletion(c.Request.Context(), token, requestID, d.now)
	if writeAccountDeletionError(c, err) {
		return
	}
	if receipt != nil {
		c.JSON(http.StatusOK, gin.H{"requestID": receipt.RequestID, "requestedAt": receipt.RequestedAt, "state": "accepted"})
	} else {
		c.JSON(http.StatusOK, gin.H{"requestID": requestID, "state": "withdrawn"})
	}
}

func (h *uploadHTTP) accountDeletionInput(c *gin.Context) (string, string, bool) {
	if h.login == nil || h.login.AccountDeletion == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "account_deletion_unavailable"})
		return "", "", false
	}
	if !h.allowLogin(c, "account-deletion") {
		return "", "", false
	}
	token, err := requestSessionToken(c.Request)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "account_authentication_required"})
		return "", "", false
	}
	kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || kind != "application/json" {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"code": "json_required"})
		return "", "", false
	}
	requestID, err := decodeAccountDeletion(c)
	if err != nil || c.Request.URL.RawQuery != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_account_deletion_request"})
		return "", "", false
	}
	return token, requestID, true
}

func writeAccountDeletionError(c *gin.Context, err error) bool {
	if errors.Is(err, ErrSession) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "account_authentication_required"})
		return true
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "account_deletion_unavailable"})
		return true
	}
	return false
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
