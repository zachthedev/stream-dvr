//go:build dev

package library

// BuildOwner is the lineage this binary may open. Builds tagged dev own
// sandbox libraries only, which keeps development off a real archive even
// when the config points at one. See owner_prod.go for the released
// counterpart.
const BuildOwner = OwnerDev
