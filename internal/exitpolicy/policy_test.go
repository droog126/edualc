package exitpolicy

import (
	"testing"
	"time"
)

func TestCheckExitCountryBlocksMainlandAndRestricted(t *testing.T) {
	// 114.114.114.114 is in mainland China CIDR list
	st, _ := CheckExitCountry("114.114.114.114", &Location{
		IP: "114.114.114.114", CountryCode: "US", Country: "Spoofed", Timezone: "America/Los_Angeles",
	})
	if st != Failed {
		t.Fatalf("expected mainland CIDR to fail even with spoofed country, got %s", st)
	}

	// Hong Kong blocked
	st, _ = CheckExitCountry("1.1.1.1", &Location{
		IP: "1.1.1.1", CountryCode: "HK", Country: "Hong Kong", Timezone: "Asia/Hong_Kong",
	})
	if st != Failed {
		t.Fatalf("expected HK to fail, got %s", st)
	}

	// US allowed
	st, _ = CheckExitCountry("8.8.8.8", &Location{
		IP: "8.8.8.8", CountryCode: "US", Country: "United States", Timezone: "America/Los_Angeles",
	})
	if st != Passed {
		t.Fatalf("expected US to pass, got %s", st)
	}
}

func TestPolicyEvaluateStaticAndDynamic(t *testing.T) {
	loc := &Location{
		IP:          "34.102.136.180",
		Country:     "United States",
		CountryCode: "US",
		Region:      "California",
		City:        "San Jose",
		Timezone:    "America/Los_Angeles",
		CheckedAt:   time.Now(),
	}

	pStatic := Policy{Mode: "static", IP: "34.102.136.180"}
	if st, msg := pStatic.Evaluate(loc.IP, loc); st != Passed {
		t.Fatalf("expected static pass, got %s (%s)", st, msg)
	}
	if st, _ := pStatic.Evaluate("8.8.8.8", &Location{IP: "8.8.8.8", CountryCode: "US", Timezone: "America/Los_Angeles"}); st != Failed {
		t.Fatalf("expected static mismatch fail, got %s", st)
	}

	pCity := Policy{Mode: "dynamic", Scope: "city", CountryCode: "US", Region: "California", City: "San Jose"}
	if st, msg := pCity.Evaluate(loc.IP, loc); st != Passed {
		t.Fatalf("expected city pass, got %s (%s)", st, msg)
	}

	pWrongCountry := Policy{Mode: "dynamic", Scope: "country", CountryCode: "JP"}
	if st, _ := pWrongCountry.Evaluate(loc.IP, loc); st != Failed {
		t.Fatalf("expected country mismatch fail, got %s", st)
	}
}
