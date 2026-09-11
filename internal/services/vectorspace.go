package services

import "strconv"

// Formatted "<provider>/<model>/<dims>"; the index stores it verbatim to detect a changed embedder.
type VectorSpace string

func NewVectorSpace(provider, model string, dims int) VectorSpace {
	return VectorSpace(provider + "/" + model + "/" + strconv.Itoa(dims))
}

func (v VectorSpace) String() string { return string(v) }
