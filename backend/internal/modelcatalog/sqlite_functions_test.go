package modelcatalog

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// The catalog tests run on SQLite, so the two helpers migration 052 defines
// for PostgreSQL are registered here with identical semantics.
var qualifiedPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}::`)

func init() {
	text := func(args []driver.Value) (string, error) {
		if len(args) != 1 {
			return "", fmt.Errorf("expected one argument")
		}
		switch v := args[0].(type) {
		case string:
			return v, nil
		case []byte:
			return string(v), nil
		case nil:
			return "", nil
		}
		return fmt.Sprint(args[0]), nil
	}
	_ = sqlite.RegisterScalarFunction("model_provider", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, err := text(args)
		if err != nil {
			return nil, err
		}
		if qualifiedPrefix.MatchString(value) {
			return strings.SplitN(value, "::", 2)[0], nil
		}
		return "openai-compatible", nil
	})
	// PostgreSQL's NOW() used by policy updates.
	_ = sqlite.RegisterScalarFunction("NOW", 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		return time.Now().UTC().Format("2006-01-02 15:04:05"), nil
	})
	_ = sqlite.RegisterScalarFunction("model_sku", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, err := text(args)
		if err != nil {
			return nil, err
		}
		if qualifiedPrefix.MatchString(value) {
			return strings.SplitN(value, "::", 2)[1], nil
		}
		return value, nil
	})
}
