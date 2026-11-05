package provision

import "testing"

func TestClusterVersionReadyUsesOpenShiftConditions(t *testing.T) {
	tests := []struct {
		name   string
		states map[string]string
		want   bool
	}{
		{
			name: "healthy",
			states: map[string]string{
				"Available":   "True",
				"Failing":     "False",
				"Progressing": "False",
			},
			want: true,
		},
		{
			name: "failing",
			states: map[string]string{
				"Available":   "True",
				"Failing":     "True",
				"Progressing": "False",
			},
		},
		{
			name: "progressing",
			states: map[string]string{
				"Available":   "True",
				"Failing":     "False",
				"Progressing": "True",
			},
		},
		{
			name: "unavailable",
			states: map[string]string{
				"Available":   "False",
				"Failing":     "False",
				"Progressing": "False",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := clusterVersionReady(test.states); got != test.want {
				t.Fatalf("clusterVersionReady(%v) = %t, want %t", test.states, got, test.want)
			}
		})
	}
}
