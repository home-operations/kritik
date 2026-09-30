package configfile

// servedAccounts is every account conns serve, in their order, with its
// entry where there is one, and the entries no connection serves.
func servedAccounts(conns []Connection, entries []Account) (served, unserved []Account) {
	byKey := map[string]*Account{}
	for i := range entries {
		byKey[entries[i].Key()] = &entries[i]
	}
	used := map[string]bool{}
	for _, in := range conns {
		for _, name := range in.Accounts {
			key := AccountKey(in.Forge, name)
			if used[key] {
				continue
			}
			used[key] = true
			if e, ok := byKey[key]; ok {
				served = append(served, *e)
			} else {
				served = append(served, Account{Forge: in.Forge, Name: name})
			}
		}
	}
	for _, e := range entries {
		if !used[e.Key()] {
			unserved = append(unserved, e)
		}
	}
	return served, unserved
}

// Unserved lists the account entries no connection serves: kept, for when
// one does again, but not run.
func (f *File) Unserved() []Account { return f.unserved }
