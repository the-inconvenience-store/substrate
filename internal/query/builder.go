package query

import (
	"fmt"
	"strconv"
	"strings"
)

var rangeOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

// colExpr returns the SQL expression for a field: a bare column for system
// fields, else a JSONB text extraction.
func colExpr(field string) string {
	if isSystemCol(field) {
		return field
	}
	return "data->>'" + field + "'"
}

// colType returns the SQL type a system column / data field sorts and keysets as.
func colType(field string) string {
	switch field {
	case "created_at", "updated_at":
		return "timestamptz"
	case "revision":
		return "bigint"
	case "id":
		return "uuid"
	default:
		return "text"
	}
}

// inferCast picks the cast for a range comparison from the raw value.
func inferCast(expr, value string) string {
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return "(" + expr + ")::numeric"
	}
	if value == "true" || value == "false" {
		return "(" + expr + ")::boolean"
	}
	return expr
}

// normalizeSort renders the sort spec back to its canonical "-field"/"field" string.
func normalizeSort(keys []SortKey) string {
	if len(keys) == 0 {
		return ""
	}
	if keys[0].Desc {
		return "-" + keys[0].Field
	}
	return keys[0].Field
}

// Build renders a ListQuery into a SQL statement. $1 and $2 are reserved for
// workspace_id and collection_id (the caller supplies them as args[0], args[1]);
// value args follow in $3.. order.
func Build(q ListQuery) (string, []any, error) {
	var b strings.Builder
	args := []any{}            // value args, starting logically at $3
	ph := func(v any) string { // append an arg, return its placeholder
		args = append(args, v)
		return "$" + strconv.Itoa(len(args)+2)
	}

	sortField := q.Sort[0].Field
	sortExpr := colExpr(sortField)
	dir := "ASC"
	cmp := ">"
	if q.Sort[0].Desc {
		dir, cmp = "DESC", "<"
	}

	b.WriteString("SELECT id, collection_id, data, revision, status, actor, created_at, (")
	b.WriteString(sortExpr)
	b.WriteString(")::text AS sort_key\n")
	b.WriteString("FROM records\n")
	b.WriteString("WHERE workspace_id = $1 AND collection_id = $2 AND status = 'active'")

	for _, f := range q.Filters {
		expr := colExpr(f.Field)
		switch f.Op {
		case "eq":
			b.WriteString("\n  AND " + expr + " = " + ph(f.Value))
		case "neq":
			b.WriteString("\n  AND " + expr + " IS DISTINCT FROM " + ph(f.Value))
		case "in":
			b.WriteString("\n  AND " + expr + " = ANY(" + ph(f.List) + ")")
		case "exists":
			if isSystemCol(f.Field) {
				return "", nil, badRequest("exists is not supported on system fields")
			}
			b.WriteString("\n  AND (data ? '" + f.Field + "') = " + ph(f.Value == "true"))
		default: // range
			op := rangeOps[f.Op]
			if isSystemCol(f.Field) {
				b.WriteString("\n  AND " + expr + " " + op + " " + ph(f.Value) + "::" + colType(f.Field))
			} else {
				b.WriteString("\n  AND " + inferCast(expr, f.Value) + " " + op + " " + ph(f.Value))
			}
		}
	}

	if q.Cursor != "" {
		c, err := decodeCursor(q.Cursor)
		if err != nil {
			return "", nil, err
		}
		if c.Sort != normalizeSort(q.Sort) {
			return "", nil, badRequest("cursor does not match sort")
		}
		valPH := ph(c.Value)
		idPH := ph(c.ID)
		b.WriteString(fmt.Sprintf("\n  AND (%s, id) %s (%s::%s, %s::uuid)",
			sortExpr, cmp, valPH, colType(sortField), idPH))
	}

	b.WriteString(fmt.Sprintf("\nORDER BY %s %s, id %s", sortExpr, dir, dir))
	b.WriteString(fmt.Sprintf("\nLIMIT %d", q.Limit+1))

	return b.String(), args, nil
}

// NextCursor builds the opaque cursor for the row that ended a page.
func NextCursor(q ListQuery, sortValue, id string) string {
	return encodeCursor(cursorData{Sort: normalizeSort(q.Sort), Value: sortValue, ID: id})
}
