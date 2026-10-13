package main

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"time"
)

// Charge the complete maximum target, not merely its increment: three 20 GiB
// volumes round up to at most 66 provider GB. Gross EUR pricing must fit within
// the approved EUR 5 monthly increment even under this conservative bound.
func priceWithinBudget(b []byte) bool {
	return priceCode(b) == "WITHIN_BUDGET"
}

// priceCode classifies bounded provider amounts without rounding prices or
// disclosing response values; the full gross-EUR target must still fit the cap.
func priceCode(b []byte) string {
	response, err := decode(b)
	gross := str(field(response, "pricing", "volume", "price_per_gb_month", "gross"))
	if err != nil {
		return "MALFORMED_RESPONSE"
	}
	if field(response, "pricing", "currency") != "EUR" {
		return "UNSUPPORTED_CURRENCY"
	}
	// The provider represents money as decimal strings, not fixed-scale floats.
	// Bound parsing work while retaining precision; never round a price down
	// to make the same gross-EUR budget pass.
	if !regexp.MustCompile(`^[0-9]{1,8}(\.[0-9]{1,32})?$`).MatchString(gross) {
		return "UNSUPPORTED_DECIMAL"
	}
	price, ok := new(big.Rat).SetString(gross)
	if !ok || price.Sign() <= 0 {
		return "NONPOSITIVE_PRICE"
	}
	if new(big.Rat).Mul(price, big.NewRat(66, 1)).Cmp(big.NewRat(5, 1)) > 0 {
		return "OVER_BUDGET"
	}
	return "WITHIN_BUDGET"
}

func fetchPrice(ctx context.Context, client *http.Client, token string) bool {
	return fetchPriceCode(ctx, client, token) == "WITHIN_BUDGET"
}

func fetchPriceCode(ctx context.Context, client *http.Client, token string) string {
	if token == "" {
		return "MISSING_CREDENTIAL"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.hetzner.cloud/v1/pricing", nil)
	if err != nil {
		return "INVALID_REQUEST"
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return "TRANSPORT_UNAVAILABLE"
	}
	b, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	closeErr := response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Sprintf("HTTP_%d", response.StatusCode)
	}
	if readErr != nil || closeErr != nil {
		return "INCOMPLETE_RESPONSE"
	}
	return priceCode(b)
}

func priceAllowed(ctx context.Context) bool {
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	code := fetchPriceCode(ctx, client, os.Getenv("HCLOUD_TOKEN"))
	if code != "WITHIN_BUDGET" {
		_, _ = fmt.Fprintln(os.Stderr, "OPENBAO_AUDIT_PRICING="+code)
	}
	return code == "WITHIN_BUDGET"
}
