package ui

import (
	"net/url"
	"strconv"
)

// Reusable list pagination. The first adopter is /ui/projects/<id>/keys
// (whose one-time task keys made the list endless), but Paginator and
// its {{template "pagination"}} partial are deliberately list-agnostic
// so Executions / Tasks / any other long list can adopt them without
// reinventing the controls. The data layer still hands back a full
// (filtered) slice today; pageWindow slices it in memory. Swapping in a
// repo-level LIMIT/OFFSET later is a data-source change only — the
// Paginator shape and the partial stay identical.

const defaultPerPage = 25

// Paginator is the template-facing pagination model. It is view-only:
// the caller has already sliced the data to the current page with
// pageWindow; Paginator renders the "Showing X–Y of Z" summary and the
// prev/next controls, preserving every other query param (status
// filters, etc.) on the navigation links.
type Paginator struct {
	Page    int // 1-based, clamped into [1, Pages]
	PerPage int
	Total   int // total items across all pages (post-filter)
	Pages   int
	Start   int // 1-based index of the first item shown (0 when empty)
	End     int // 1-based index of the last item shown
	HasPrev bool
	HasNext bool
	PrevURL string
	NextURL string
	// Unknown is set when the total could not be counted: the summary says
	// so and names no total, and Next is offered while pages come back full
	// (a failed count is never a total; web-ui-design.md, #411 item 4).
	Unknown bool
	// Label names the controls' <nav> landmark ("Pagination" when empty);
	// Noun follows the total in the summary ("of 130 tasks"). Both optional.
	Label string
	Noun  string
}

// NewPaginator clamps page into [1, Pages], computes the visible
// window, and builds prev/next URLs from basePath + the supplied query
// values. The "page" key is overwritten; every other key is preserved
// so an active/all filter survives navigation. perPage <= 0 falls back
// to defaultPerPage. Pass the SAME page value to pageWindow so the
// slice and the controls agree.
func NewPaginator(total, page, perPage int, basePath string, query url.Values) Paginator {
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}

	p := Paginator{Page: page, PerPage: perPage, Total: total, Pages: pages}
	if total > 0 {
		p.Start = (page-1)*perPage + 1
		p.End = page * perPage
		if p.End > total {
			p.End = total
		}
	}
	p.HasPrev = page > 1
	p.HasNext = page < pages

	p.setURLs(basePath, query)
	return p
}

// NewUnknownTotalPaginator is NewPaginator for a list whose total could not
// be counted: the caller listed page `page` and got `shown` rows. Next is
// offered when the page came back full; no total or page count is claimed.
func NewUnknownTotalPaginator(page, perPage, shown int, basePath string, query url.Values) Paginator {
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if page < 1 {
		page = 1
	}
	p := Paginator{Page: page, PerPage: perPage, Unknown: true}
	if shown > 0 {
		p.Start = (page-1)*perPage + 1
		p.End = p.Start + shown - 1
	}
	p.HasPrev = page > 1
	p.HasNext = shown >= perPage
	p.setURLs(basePath, query)
	return p
}

// setURLs builds PrevURL/NextURL from basePath and query, overwriting only
// "page" so every other filter survives navigation.
func (p *Paginator) setURLs(basePath string, query url.Values) {
	mk := func(n int) string {
		q := url.Values{}
		for k, v := range query {
			if k == "page" {
				continue
			}
			q[k] = append([]string(nil), v...)
		}
		q.Set("page", strconv.Itoa(n))
		return basePath + "?" + q.Encode()
	}
	if p.HasPrev {
		p.PrevURL = mk(p.Page - 1)
	}
	if p.HasNext {
		p.NextURL = mk(p.Page + 1)
	}
}

// pageWindow slices items to the 1-based page. An out-of-range page
// clamps to the last page's window (matching NewPaginator's clamp) so
// a stale ?page= never renders an empty page mid-list. perPage <= 0
// falls back to defaultPerPage.
func pageWindow[T any](items []T, page, perPage int) []T {
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if len(items) == 0 {
		return items
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * perPage
	if start >= len(items) {
		lastPage := (len(items) + perPage - 1) / perPage
		start = (lastPage - 1) * perPage
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// parsePageParam reads the 1-based ?page= query value, defaulting to 1
// for absent / malformed / non-positive input.
func parsePageParam(values url.Values) int {
	if n, err := strconv.Atoi(values.Get("page")); err == nil && n > 0 {
		return n
	}
	return 1
}
