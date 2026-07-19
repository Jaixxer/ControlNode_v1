package database

import "gorm.io/gorm"

type Workers struct {
	gorm.Model
}

type Deployments struct {
	gorm.Model
	GithubURL string
	WorkerID  uint
	Workers   Workers `gorm:"foreignKey:WorkerID"`
}
