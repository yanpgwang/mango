package mango

import (
	"context"
	"errors"
)

// Page is the common projection of either a next_page response or Files cursor.
type Page[T any] struct {
	Data []T
	Next string
}

// PageIterator fetches lazily and detects repeated cursors. Generated *AutoPaging
// methods preserve filters and use each resource's own cursor convention.
type PageIterator[T any] struct {
	ctx     context.Context
	fetch   func(context.Context, string) (Page[T], error)
	cursor  string
	seen    map[string]bool
	items   []T
	index   int
	started bool
	done    bool
	current T
	err     error
}

func NewPageIterator[T any](ctx context.Context, firstCursor string, fetch func(context.Context, string) (Page[T], error)) *PageIterator[T] {
	return &PageIterator[T]{ctx: ctx, fetch: fetch, cursor: firstCursor, seen: make(map[string]bool)}
}

func (p *PageIterator[T]) Next() bool {
	for p.err == nil {
		if err := p.ctx.Err(); err != nil {
			p.err = err
			return false
		}
		if p.index < len(p.items) {
			p.current = p.items[p.index]
			p.index++
			return true
		}
		if p.done {
			return false
		}
		if p.started && p.seen[p.cursor] {
			p.err = errors.New("mango: pagination cursor repeated")
			return false
		}
		p.seen[p.cursor], p.started = true, true
		page, err := p.fetch(p.ctx, p.cursor)
		if err != nil {
			p.err = err
			return false
		}
		p.items, p.index, p.cursor, p.done = page.Data, 0, page.Next, page.Next == ""
	}
	return false
}

func (p *PageIterator[T]) Value() T   { return p.current }
func (p *PageIterator[T]) Err() error { return p.err }
