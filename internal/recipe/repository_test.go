package recipe

import "testing"

func TestRepositoryIdentityNormalizesGitHubURLAndPath(t *testing.T) {
	id, sourceURL, sourcePath, err := RepositoryIdentity(Source{
		URL:  "HTTPS://GitHub.com/MiaAI-Lab/Qwen.git/",
		Path: "recipes/../runtime/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sourceURL != "https://github.com/MiaAI-Lab/Qwen" {
		t.Fatalf("source URL = %q", sourceURL)
	}
	if sourcePath != "runtime" {
		t.Fatalf("source path = %q", sourcePath)
	}
	if id != "repo-68747470733a2f2f6769746875622e636f6d2f4d696141492d4c61622f5177656e0a72756e74696d65" {
		t.Fatalf("identity = %q", id)
	}
}

func TestRepositoryIdentityKeepsIdentityBoundaries(t *testing.T) {
	_, httpsURL, rootPath, err := RepositoryIdentity(Source{URL: "https://github.com/Acme/Model.git"})
	if err != nil {
		t.Fatal(err)
	}
	_, sshURL, _, err := RepositoryIdentity(Source{URL: "git@github.com:Acme/Model.git"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, nestedPath, err := RepositoryIdentity(Source{URL: "https://github.com/Acme/Model", Path: "nested"})
	if err != nil {
		t.Fatal(err)
	}
	if httpsURL == sshURL {
		t.Fatalf("HTTPS and SSH identities merged: %q", httpsURL)
	}
	if rootPath == nestedPath {
		t.Fatalf("root and nested source paths merged: %q", rootPath)
	}
}
