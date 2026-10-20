package main

import (
	"embed"
	"io/fs"

	"noderouter/proxy"
	"noderouter/router"
)

// The static site and the user guides live at the module root, so the embed
// directives have to live here too: //go:embed can only reach files under the
// package's own directory, and the website is deliberately kept at ./website
// (css/, js/, img/, fonts/, favicon/), not under a Go package.
//
//go:embed website
var websiteFS embed.FS

//go:embed docs
var docsFS embed.FS

//go:embed README.md
var readmeContent []byte

func init() {
	site, err := fs.Sub(websiteFS, "website")
	if err != nil {
		panic("website embed: " + err.Error())
	}
	docs, err := fs.Sub(docsFS, "docs")
	if err != nil {
		panic("docs embed: " + err.Error())
	}
	router.SetSiteFS(site, docs, readmeContent)

	// The build version is stamped on main; hand it to the packages that
	// report it (the index endpoint and the proxy-source User-Agent).
	proxy.Version = Version
	router.Version = Version
}
