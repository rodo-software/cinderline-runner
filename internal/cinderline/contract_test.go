package cinderline

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestContractFileMatchesPinnedHash(t *testing.T) {
	b, err := os.ReadFile("../../docs/runner-api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != contractSHA256 {
		t.Errorf("docs/runner-api/openapi.yaml has SHA-256 %s, contractSHA256 says %s: copy the file from the app repository unchanged, check the types against it and update the constant in the same commit", got, contractSHA256)
	}
}
