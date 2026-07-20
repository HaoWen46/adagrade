package auth

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewSessionManager builds the server-side session manager backed by the sessions
// table (spec §2: instant revocation; pgxstore cleans expired rows every 5 minutes).
// secure toggles the cookie Secure flag — true in production, false for local http.
func NewSessionManager(pool *pgxpool.Pool, secure bool) *scs.SessionManager {
	sm := scs.New()
	sm.Store = pgxstore.New(pool)
	sm.Lifetime = 7 * 24 * time.Hour
	sm.Cookie.Name = "ada_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.Secure = secure
	sm.Cookie.SameSite = http.SameSiteLaxMode
	return sm
}
