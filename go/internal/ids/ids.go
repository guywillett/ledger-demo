// Package ids holds the entity IDs. Inside the database every ID is a UUIDv7 (a uuid column);
// everywhere else (JSON, events, logs) it is a TypeID such as "dep_01m5…", whose prefix names
// the entity. Conversion happens only at those boundaries. Node: node/src/shared/ids.ts.
//
// Each entity has its own Go type (AccountID, DepositID, …), so passing a deposit's ID where an
// account's is expected doesn't compile, as Node's Uuid<'acct'> and Uuid<'dep'> don't mix.
//
// Not the usual Go way (decision 54): the five types are one generic type, ID[K], told apart by a
// "phantom" type parameter K that is never stored, only used to pick the prefix. The common Go
// alternative is five separate types with the same seven methods each; the generic type writes
// them once. typeid-go uses the same pattern for its own TypeID[P].
package ids

import (
	"database/sql/driver"
	"fmt"

	"github.com/google/uuid"
	"go.jetify.com/typeid"
)

// Kind is implemented by the entity marker types below and by nothing else: its method is
// unexported, so no other package can add a kind.
type Kind interface {
	prefix() string
}

// The entity kinds: empty structs that only carry their TypeID prefix.
type (
	Account    struct{}
	Deposit    struct{}
	Withdrawal struct{}
	Event      struct{}
	Movement   struct{}
)

func (Account) prefix() string    { return "acct" }
func (Deposit) prefix() string    { return "dep" }
func (Withdrawal) prefix() string { return "wd" }
func (Event) prefix() string      { return "evt" }
func (Movement) prefix() string   { return "mov" }

// ID is the UUID of one kind of entity. The field is unexported, so outside this package an ID
// comes only from New, FromTypeID or the database (Scan); an ID of one kind can still be
// converted to another explicitly, ids.DepositID(accountID), as Node's `as` can.
type ID[K Kind] struct {
	uuid uuid.UUID
}

// The ID types used everywhere else. Type aliases (=), so AccountID is ID[Account] itself, not a
// new type: its methods are ID's.
type (
	AccountID    = ID[Account]
	DepositID    = ID[Deposit]
	WithdrawalID = ID[Withdrawal]
	EventID      = ID[Event]
	MovementID   = ID[Movement]
)

// New returns a new UUIDv7 ID, for inserting a new row: ids.New[ids.Deposit]().
func New[K Kind]() ID[K] {
	// NewV7 fails only if reading randomness fails, and since Go 1.24 crypto/rand never returns
	// an error (it crashes the program instead), so Must can't panic here.
	return ID[K]{uuid: uuid.Must(uuid.NewV7())}
}

// FromTypeID parses a TypeID from outside: ids.FromTypeID[ids.Deposit]("dep_01m5…"). It must
// be well formed, have K's prefix and hold a UUIDv7, as Node's fromTypeId checks.
func FromTypeID[K Kind](s string) (ID[K], error) {
	var k K
	tid, err := typeid.FromString(s)
	if err != nil {
		return ID[K]{}, fmt.Errorf("invalid TypeID: %w", err)
	}
	if tid.Prefix() != k.prefix() {
		return ID[K]{}, fmt.Errorf("TypeID has prefix %q, want %q", tid.Prefix(), k.prefix())
	}
	u, err := uuid.FromBytes(tid.UUIDBytes())
	if err != nil {
		return ID[K]{}, fmt.Errorf("invalid TypeID: %w", err)
	}
	if u.Version() != 7 {
		return ID[K]{}, fmt.Errorf("TypeID holds a version %d UUID, want 7", u.Version())
	}
	return ID[K]{uuid: u}, nil
}

// TypeID returns the ID as a TypeID string, e.g. "dep_01m5…".
func (id ID[K]) TypeID() string {
	var k K
	// The prefixes above are valid TypeID prefixes, so this can't fail.
	return typeid.Must(typeid.FromUUIDBytesWithPrefix(k.prefix(), id.uuid[:])).String()
}

// String makes fmt and log output show the TypeID, as Node's logs do.
func (id ID[K]) String() string {
	return id.TypeID()
}

// UUID returns the database form, e.g. for comparing with a row read some other way in tests.
func (id ID[K]) UUID() uuid.UUID {
	return id.uuid
}

// IsZero reports whether the ID was never set (the all-zero UUID).
func (id ID[K]) IsZero() bool {
	return id.uuid == uuid.Nil
}

// MarshalText and UnmarshalText (the encoding.TextMarshaler pair) make encoding/json write an ID
// as its TypeID and read one by parsing it: decoding a request body into a struct with a
// DepositID field validates the field, as a Zod schema with typeIdSchema('dep') does in Node.
func (id ID[K]) MarshalText() ([]byte, error) {
	return []byte(id.TypeID()), nil
}

// UnmarshalText parses a TypeID; see MarshalText.
func (id *ID[K]) UnmarshalText(text []byte) error {
	parsed, err := FromTypeID[K](string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Value (database/sql/driver.Valuer) and Scan (sql.Scanner) make pgx store and read an ID as its
// UUID: in the database an ID is a uuid, never a TypeID.
func (id ID[K]) Value() (driver.Value, error) {
	return id.uuid.String(), nil
}

// Scan reads a uuid column; see Value.
func (id *ID[K]) Scan(src any) error {
	var u uuid.UUID
	if err := u.Scan(src); err != nil {
		return err
	}
	id.uuid = u
	return nil
}
