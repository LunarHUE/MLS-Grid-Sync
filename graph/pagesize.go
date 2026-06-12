package graph

// MaxPageSize caps relay first/last and is the implicit page size when
// a list query passes neither. Without it entgql applies no LIMIT at
// all (ent/gql_pagination.go: paginateLimit returns 0).
const MaxPageSize = 500

func clampPage(first, last *int) (*int, *int) {
	if first == nil && last == nil {
		def := MaxPageSize
		return &def, nil
	}
	if first != nil && *first > MaxPageSize {
		c := MaxPageSize
		first = &c
	}
	if last != nil && *last > MaxPageSize {
		c := MaxPageSize
		last = &c
	}
	return first, last
}
