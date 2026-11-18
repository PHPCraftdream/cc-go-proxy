package conn

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

func testSession(t *testing.T) *oauth.Session {
	t.Helper()
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}
