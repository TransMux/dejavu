package dejavu

import "testing"

func TestCloudLockAttempts(t *testing.T) {
	if got := cloudLockAttempts(nil); 1 != got {
		t.Fatalf("automatic lock attempts = %d, want 1", got)
	}
	if got := cloudLockAttempts(map[string]interface{}{"manualSync": false}); 1 != got {
		t.Fatalf("explicit automatic lock attempts = %d, want 1", got)
	}
	if got := cloudLockAttempts(map[string]interface{}{"manualSync": true}); 2 != got {
		t.Fatalf("manual lock attempts = %d, want 2", got)
	}
}
