package source

import "testing"

func TestParseRepoURLWithBranch(t *testing.T) {
	for _, tc := range []struct {
		name, url, branch, want string
		confirmed, valid        bool
	}{
		{"plain default", "github.com/Owner/Repo", "", "main", false, true},
		{"case preserved", "https://github.com/Owner/Repo", "Feature/X", "Feature/X", false, true},
		{"tree unconfirmed", "https://github.com/Owner/Repo/tree/main", "main", "", false, false},
		{"tree omitted", "https://github.com/Owner/Repo/tree/main", "", "", true, false},
		{"tree mismatch", "https://github.com/Owner/Repo/tree/feature/x", "main", "", true, false},
		{"whole segment", "https://github.com/Owner/Repo/tree/featurex", "feature", "", true, false},
		{"slash branch", "https://github.com/Owner/Repo/tree/feature/x/src", "feature/x", "feature/x", true, true},
		{"blob branch", "https://github.com/Owner/Repo/blob/Feature/X/README.md", "Feature/X", "Feature/X", true, true},
		{"invalid suffix", "https://github.com/Owner/Repo/issues/1", "main", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var branch *string
			if tc.branch != "" {
				branch = &tc.branch
			}
			repo, err := ParseRepoURLWithBranch(tc.url, branch, tc.confirmed)
			if (err == nil) != tc.valid || (tc.valid && repo.Branch != tc.want) {
				t.Fatalf("repo=%+v err=%v", repo, err)
			}
		})
	}
}
