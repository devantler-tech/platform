// Command check-business-disclosure-reader validates one rendered bootstrap payload.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/devantler-tech/platform/internal/strictjson"
)

func expectedPayload(kind string) string {
	switch kind {
	case "role":
		return `{
"bound_service_account_names":["business-site"],
"bound_service_account_namespaces":["business-site"],
"token_policies":["app-business-site-disclosure-readonly"],
"token_no_default_policy":true,"token_ttl":3600,"token_max_ttl":3600}`
	case "policy":
		return `{"path":{
"secret/data/apps/business-site/public-disclosure":{"capabilities":["read"]},
"auth/token/lookup-self":{"capabilities":["read"]},
"auth/token/revoke-self":{"capabilities":["update"]}}}`
	default:
		return ""
	}
}

func run(args []string, input io.Reader) error {
	if len(args) != 1 || expectedPayload(args[0]) == "" {
		return errors.New("expected role or policy")
	}
	const maxBytes = 16 << 10
	body, err := io.ReadAll(io.LimitReader(input, maxBytes+1))
	if err != nil || len(body) > maxBytes {
		return errors.New("unreadable or oversized payload")
	}
	// The same maintained parser used for other platform evidence rejects both
	// duplicate members and trailing documents before a map decode can erase them.
	if err := strictjson.Validate(body, nil); err != nil {
		return errors.New("ambiguous or malformed payload")
	}
	var actual, expected any
	if json.Unmarshal(body, &actual) != nil || json.Unmarshal([]byte(expectedPayload(args[0])), &expected) != nil || !reflect.DeepEqual(actual, expected) {
		return errors.New("payload differs from the dedicated reader contract")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
