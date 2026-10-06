package usage

import (
	_ "embed"
	"regexp"
)

//go:embed prices.json
var priceJSON []byte

type price struct {
	Input, Output, Read, Write float64
	Hour                       float64 `json:"write_hour"`
}
type priceTable struct {
	Version string           `json:"price_version"`
	Models  map[string]price `json:"models"`
}

var prices = decodePrices(priceJSON)
var datedModel = regexp.MustCompile(`-(?:[0-9]{8}|[0-9]{4}-[0-9]{2}-[0-9]{2})$`)

func cost(model string, t Tokens) (float64, bool) {
	p, ok := prices.Models[model]
	if !ok {
		p, ok = prices.Models[datedModel.ReplaceAllString(model, "")]
	}
	if !ok {
		return 0, true
	}
	return (t.InputTokens*p.Input + t.OutputTokens*p.Output + t.CacheReadTokens*p.Read + t.CacheWriteTokens*p.Write + t.CacheWriteHourTokens*p.Hour) / 1e6, false
}
