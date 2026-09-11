package redis

import (
	"fmt"

	"github.com/google/uuid"
)

// parseUUID turns a stored identifier back into a UUID.
//
// A malformed value here means the cache holds something this code did not
// write, so it is an error rather than a zero value: a nil workspace id would
// pass through the policies as "no tenant" and quietly match nothing.
func parseUUID(value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("decode identifier %q: %w", value, err)
	}
	return parsed, nil
}
