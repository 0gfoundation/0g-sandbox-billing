package web

import _ "embed"

//go:embed dashboard.html
var DashboardHTML []byte

//go:embed user.html
var UserHTML []byte

//go:embed provider.html
var ProviderHTML []byte

//go:embed ethers.umd.min.js
var EthersJS []byte

//go:embed logo.svg
var LogoSVG []byte
