package memory

import (
	"testing"

	"github.com/go-fynx/reporter/internal/processor/processortest"
)

func TestContract(t *testing.T) {
	s := New()
	processortest.Run(t, s, func(_ *testing.T, project string) int { return s.Count(project) })
}
