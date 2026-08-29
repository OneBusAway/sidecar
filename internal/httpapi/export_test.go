package httpapi

import "net/http"

// RequestLogForTest exposes requestLog to external tests that need to wrap
// a handler of their own (the panic-after-headers contract).
func RequestLogForTest(deps Deps, next http.Handler) http.Handler { return requestLog(deps, next) }
