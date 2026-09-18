package codex

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The same synthetic records are consumed by the Node filter's tests. Running
// the filter twice asserts that Node's canonical output remains valid in Go.
func TestPiSharedMetadataContract(t *testing.T) {
	raw, err := os.ReadFile("../../../../integration/pi/testdata/metadata-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string            `json:"name"`
		Input    map[string]string `json:"input"`
		Expected map[string]string `json:"expected"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("metadata fixture is empty")
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			actual := piSafeMetadata(tc.Input)
			if !reflect.DeepEqual(actual, tc.Expected) {
				t.Fatalf("metadata = %#v, want %#v", actual, tc.Expected)
			}
			if again := piSafeMetadata(actual); !reflect.DeepEqual(again, actual) {
				t.Fatalf("canonical metadata rejected on second boundary: %#v -> %#v", actual, again)
			}
		})
	}
}

func TestPiMetadataConflictingHeaderCasingFailsClosed(t *testing.T) {
	actual := piSafeMetadata(map[string]string{
		"X-Codex-Primary-Used-Percent": "20",
		"x-codex-primary-used-percent": "80",
	})
	if len(actual) != 0 {
		t.Fatalf("ambiguous casing selected a nondeterministic value: %#v", actual)
	}
}
