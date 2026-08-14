//go:build !dev

package library

// BuildOwner is the lineage this binary may open. Released builds own
// production libraries. See owner_dev.go for the sandbox counterpart.
const BuildOwner = OwnerProd
