package autopilot

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v3"
	"github.com/decred/dcrd/dcrutil/v4"
	"github.com/stretchr/testify/require"
)

// testGraphDesc is a helper type to describe a test graph.
type testGraphDesc struct {
	nodes int
	edges map[int][]int
}

var centralityTestGraph = testGraphDesc{
	nodes: 9,
	edges: map[int][]int{
		0: {1, 2, 3},
		1: {2},
		2: {3},
		3: {4, 5},
		4: {5, 6, 7},
		5: {6, 7},
		6: {7, 8},
	},
}

var testGraphCentrality = []float64{
	3.0, 0.0, 3.0, 15.0, 6.0, 6.0, 7.0, 0.0, 0.0,
}

var normalizedTestGraphCentrality = []float64{
	0.2, 0.0, 0.2, 1.0, 0.4, 0.4, 7.0 / 15.0, 0.0, 0.0,
}

// buildTestGraph builds a test graph from a passed graph desriptor.
func buildTestGraph(t *testing.T,
	graph testGraph, desc testGraphDesc) map[int]*secp256k1.PublicKey {

	nodes := make(map[int]*secp256k1.PublicKey)

	for i := 0; i < desc.nodes; i++ {
		key, err := graph.addRandNode()
		require.NoError(t, err, "cannot create random node")

		nodes[i] = key
	}

	const chanCapacity = dcrutil.AtomsPerCoin
	for u, neighbors := range desc.edges {
		for _, v := range neighbors {
			_, _, err := graph.addRandChannel(
				nodes[u], nodes[v], chanCapacity,
			)
			require.NoError(t, err,
				"unexpected error adding random channel",
			)
			if err != nil {
				t.Fatalf("unexpected error adding"+
					"random channel: %v", err)
			}
		}
	}

	return nodes
}
