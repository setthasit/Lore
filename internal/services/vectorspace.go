package services

import "strconv"

// VectorSpace identifies the vector space a workspace is configured for,
// formatted "<provider>/<model>/<dims>". The host composes it — from the
// provider's registered name, the configured model, and the width the embedder
// reports — so a provider cannot claim another's identity and quietly poison an
// index whose vectors mean something else.
type VectorSpace string

func NewVectorSpace(provider, model string, dims int) VectorSpace {
	return VectorSpace(provider + "/" + model + "/" + strconv.Itoa(dims))
}

func (v VectorSpace) String() string { return string(v) }
