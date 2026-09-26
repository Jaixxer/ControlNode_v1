package database

import (
	"strings"
	"time"

	"gorm.io/gorm"
)

// Workers is a worker node known to the control plane. The row survives worker
// restarts so its last seen time and status can be tracked across connections.
type Workers struct {
	gorm.Model
	// Name is the worker's identity, taken from its bootstrap token.
	Name string `gorm:"index"`
	// LastSeen is the last time the worker reported in.
	LastSeen time.Time
	// Status is cluster.StatusOnline or cluster.StatusOffline.
	Status string
}

type Deployments struct {
	gorm.Model
	GithubURL string
	WorkerID  uint
	Workers   Workers `gorm:"foreignKey:WorkerID"`
}

// GithubCredential stores the GitHub account the control plane is linked to.
// There is no user model yet, so a single row acts as "the" credential.
type GithubCredential struct {
	gorm.Model
	GithubID    int64
	GithubLogin string
	AccessToken string
}

// WatchedRepo is a repository whose pushes should be deployed automatically.
type WatchedRepo struct {
	gorm.Model
	RepoFullName string
	// Project is the name the deploy is published under.
	Project string
	// Workers is a comma separated list of worker names. Empty means every
	// connected worker, which is the default.
	Workers string
	// Branch is the ref to deploy, e.g. "main". Empty means the default branch.
	Branch string
}

// WorkerList splits the stored worker names into a list. An empty result means
// "all workers".
func (w *WatchedRepo) WorkerList() []string {
	var out []string
	for _, name := range strings.Split(w.Workers, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}
