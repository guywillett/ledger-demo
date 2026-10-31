package compliance

import (
	"context"
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/db"
	"github.com/guywillett/ledger-demo/go/internal/deposits"
	"github.com/guywillett/ledger-demo/go/internal/ids"
)

// Decide refuses anything but an approval or a rejection with a reason, before touching the
// database (so a zero db.Tx will do). Inside the package: only it can build a Decision with another
// transition, by mistake.
func TestDecideRefusesWhatIsntADecision(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		want     string
	}{
		{"the zero Decision", Decision{}, "invalid decision"},
		{"another transition", Decision{transition: deposits.Credit}, "invalid decision"},
		{"a rejection without a reason", Rejected(""), "a rejection needs a reason"},
	}
	for _, tt := range tests {
		_, err := Decide(context.Background(), db.Tx{}, ids.New[ids.Deposit](), tt.decision)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}
