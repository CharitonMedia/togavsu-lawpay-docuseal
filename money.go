package lawpaydocuseal

import (
	"errors"
	"strconv"
	"strings"

	"github.com/CharitonMedia/togavsu-lawpay-docuseal/adapters"
)

var errAmount = errors.New("lawpay-docuseal: enter an amount like 250 or 250.00")

// ParseDollars reads an amount a person typed (250, 250.5, 250.00, $1,250.00) into whole
// cents without floating point. A negative amount, more than two decimals or anything
// else is refused.
func ParseDollars(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0, errAmount
	}
	whole, frac, dot := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if dot && (frac == "" || len(frac) > centDigits) {
		return 0, errAmount
	}
	for len(frac) < centDigits {
		frac += "0"
	}
	w, err := adapters.ParseCents(whole)
	if err != nil {
		return 0, errAmount
	}
	f, err := adapters.ParseCents(frac)
	if err != nil {
		return 0, errAmount
	}
	if w > (1<<62)/centsPerDollar {
		return 0, errAmount
	}
	return w*centsPerDollar + f, nil
}

// plainDollars formats cents as 1234.56 for a form field.
func plainDollars(c int64) string {
	return strconv.FormatInt(c/centsPerDollar, decimalBase) + "." + leftPad(strconv.FormatInt(c%centsPerDollar, decimalBase))
}

func leftPad(s string) string {
	if len(s) < centDigits {
		return "0" + s
	}
	return s
}
