// Package recovery provides panic-recovery middleware. It's Transacta's
// own replacement for chi's stock middleware.Recoverer: same recover-and-
// respond-500 behavior, but logging goes through the same structured
// slog.Logger as every other request log line -- request ID, stack
// trace, and the panic value all as structured fields -- instead of chi
// writing an unstructured trace straight to stderr.
package recovery

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/go-chi/chi/v5/middleware"
)

// Middleware recovers panics in any downstream handler or middleware,
// logs them with logger, and responds 500 instead of letting the panic
// unwind past net/http and (depending on Go version/config) take the
// whole process down.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is net/http's own sentinel for
					// "stop this handler, but don't treat it as an error" --
					// re-panicking with it lets the standard library's
					// connection-level handling deal with it silently,
					// same as chi's Recoverer does.
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}

					logger.Error("panic recovered",
						"request_id", middleware.GetReqID(r.Context()),
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rec,
						"stack", string(debug.Stack()),
					)

					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"internal server error"}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
