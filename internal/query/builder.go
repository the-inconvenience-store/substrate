package query

import (
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
	args := make([]any, 0, len(q.Filters)+2) // value args, starting logically at $3
	// writePH appends an arg and writes its placeholder ($N) directly to b,
	// avoiding the intermediate string a "$"+itoa concatenation would allocate.
	writePH := func(v any) {
		args = append(args, v)
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(len(args) + 2))
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
			b.WriteString("\n  AND ")
			b.WriteString(expr)
			b.WriteString(" = ")
			writePH(f.Value)
		case "neq":
			b.WriteString("\n  AND ")
			b.WriteString(expr)
			b.WriteString(" IS DISTINCT FROM ")
			writePH(f.Value)
		case "in":
			b.WriteString("\n  AND ")
			b.WriteString(expr)
			b.WriteString(" = ANY(")
			writePH(f.List)
			b.WriteByte(')')
		case "exists":
			if isSystemCol(f.Field) {
				return "", nil, badRequest("exists is not supported on system fields")
			}
			b.WriteString("\n  AND (data ? '")
			b.WriteString(f.Field)
			b.WriteString("') = ")
			writePH(f.Value == "true")
		default: // range
			op := rangeOps[f.Op]
			if isSystemCol(f.Field) {
				b.WriteString("\n  AND ")
				b.WriteString(expr)
				b.WriteByte(' ')
				b.WriteString(op)
				b.WriteByte(' ')
				writePH(f.Value)
				b.WriteString("::")
				b.WriteString(colType(f.Field))
			} else {
				b.WriteString("\n  AND ")
				b.WriteString(inferCast(expr, f.Value))
				b.WriteByte(' ')
				b.WriteString(op)
				b.WriteByte(' ')
				writePH(f.Value)
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
		b.WriteString("\n  AND (")
		b.WriteString(sortExpr)
		b.WriteString(", id) ")
		b.WriteString(cmp)
		b.WriteString(" (")
		writePH(c.Value)
		b.WriteString("::")
		b.WriteString(colType(sortField))
		b.WriteString(", ")
		writePH(c.ID)
		b.WriteString("::uuid)")
	}

	b.WriteString("\nORDER BY ")
	b.WriteString(sortExpr)
	b.WriteByte(' ')
	b.WriteString(dir)
	b.WriteString(", id ")
	b.WriteString(dir)
	b.WriteString("\nLIMIT ")
	b.WriteString(strconv.Itoa(q.Limit + 1))

	return b.String(), args, nil
}

// NextCursor builds the opaque cursor for the row that ended a page.
func NextCursor(q ListQuery, sortValue, id string) string {
	return encodeCursor(cursorData{Sort: normalizeSort(q.Sort), Value: sortValue, ID: id})
}
