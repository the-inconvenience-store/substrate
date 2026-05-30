// Package query parses, builds, and paginates record list queries, and manages
// the JSONB expression indexes that back them.
package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/substrate/substrate/internal/apierr"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

var (
	fieldRe    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	validOps   = map[string]bool{"eq": true, "neq": true, "gt": true, "gte": true, "lt": true, "lte": true, "in": true, "exists": true}
	systemCols = map[string]bool{"created_at": true, "updated_at": true, "revision": true, "id": true}
)

// Filter is one parsed predicate.
type Filter struct {
	Field string
	Op    string
	Value string   // raw value for scalar ops; "true"/"false" for exists
	List  []string // populated for the "in" op
}

// SortKey is one ORDER BY term.
type SortKey struct {
	Field string
	Desc  bool
}

// ListQuery is the parsed, validated representation of a list request.
type ListQuery struct {
	Filters []Filter
	Sort    []SortKey
	Limit   int
	Cursor  string // raw opaque cursor token (decoded by the builder)
}

func badRequest(msg string) error { return apierr.New(apierr.BadRequest, msg) }

// Parse validates raw query params into a ListQuery. filters is the repeated
// "filter" param; sort/limit/cursor are their single string forms ("" = unset).
func Parse(filters []string, sort, limit, cursor string) (ListQuery, error) {
	q := ListQuery{Limit: defaultLimit, Cursor: cursor}

	for _, raw := range filters {
		parts := strings.SplitN(raw, ":", 3)
		if len(parts) != 3 {
			return ListQuery{}, badRequest(fmt.Sprintf("filter %q must be field:op:value", raw))
		}
		field, op, val := parts[0], parts[1], parts[2]
		if err := checkField(field); err != nil {
			return ListQuery{}, err
		}
		if !validOps[op] {
			return ListQuery{}, badRequest(fmt.Sprintf("unknown filter op %q", op))
		}
		f := Filter{Field: field, Op: op, Value: val}
		switch op {
		case "in":
			f.List = strings.Split(val, ",")
		case "exists":
			if val != "true" && val != "false" {
				return ListQuery{}, badRequest("exists value must be true or false")
			}
		}
		q.Filters = append(q.Filters, f)
	}

	if sort == "" {
		q.Sort = []SortKey{{Field: "created_at", Desc: true}}
	} else {
		field, desc := sort, false
		if strings.HasPrefix(sort, "-") {
			field, desc = sort[1:], true
		}
		if err := checkField(field); err != nil {
			return ListQuery{}, err
		}
		q.Sort = []SortKey{{Field: field, Desc: desc}}
	}

	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n <= 0 {
			return ListQuery{}, badRequest("limit must be a positive integer")
		}
		if n > maxLimit {
			n = maxLimit
		}
		q.Limit = n
	}

	return q, nil
}

func checkField(field string) error {
	if systemCols[field] || fieldRe.MatchString(field) {
		return nil
	}
	return badRequest(fmt.Sprintf("invalid field name %q", field))
}

// isSystemCol reports whether a field maps to a real records column rather than
// a JSON path. Used by the builder.
func isSystemCol(field string) bool { return systemCols[field] }
