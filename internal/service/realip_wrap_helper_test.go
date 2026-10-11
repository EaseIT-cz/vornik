package service

import "net/http"

// wrapRealIP is a TEST helper: build the real-IP middleware once and wrap
// next. Production code calls realIPMiddleware once per initHTTPServer pass
// (GitHub #95; the production wrapRealIP was removed so the warn-once-per-
// pass rule is structural).
func (c *Container) wrapRealIP(next http.Handler) (http.Handler, error) {
	mw, err := c.realIPMiddleware()
	if err != nil {
		return nil, err
	}
	return mw(next), nil
}
