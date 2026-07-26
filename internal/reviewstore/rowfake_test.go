package reviewstore

// fakeRow is a hand-written pgx.Row (a single-method interface: Scan) used
// to make a querierMock's QueryRowFunc return a chosen error from Scan,
// so Submit/OpenRound/CurrentRound's error-mapping can be unit-tested
// without a live database.
type fakeRow struct {
	err error
}

func (r fakeRow) Scan(_ ...any) error {
	return r.err
}
