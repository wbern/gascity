package bdroute

import "testing"

func TestResolveUsesExplicitAPIAndCityPath(t *testing.T) {
	lookup := func(key string) string {
		switch key {
		case "GC_API_URL":
			return "http://127.0.0.1:7777/"
		case "GC_CITY_PATH":
			return "/srv/gc2/"
		default:
			return ""
		}
	}
	got, ok := Resolve("", lookup, nil)
	if !ok {
		t.Fatal("Resolve() = not ok")
	}
	if got.BaseURL != "http://127.0.0.1:7777" || got.City != "gc2" {
		t.Fatalf("Resolve() = %+v, want http://127.0.0.1:7777/gc2", got)
	}
}
