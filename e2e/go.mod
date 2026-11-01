module github.com/ZacxDev/clanksquare/e2e

go 1.22

require (
	github.com/ZacxDev/clankmail v0.0.0
	github.com/ZacxDev/clankmail/go/hub v0.0.0-20261101080649-c65f52f77152
	github.com/ZacxDev/clanksquare v0.0.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20230315142452-642cacee5cc0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	modernc.org/libc v1.61.13 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.8.2 // indirect
	modernc.org/sqlite v1.36.0 // indirect
)

// The curator under test is this repo's own root module, not a published copy.
replace github.com/ZacxDev/clanksquare => ../

// The hub module's go.mod requires github.com/ZacxDev/clankmail v0.0.0 (it
// builds against its repo root through a local replace, which consumers do
// not see). v0.0.0 is not a real revision, so map it to clankmail main
// c65f52f, the commit the root module requires. A pseudo-version is a
// prerelease of v0.0.0 and sorts below it, which is why the require above
// reads v0.0.0. Test-only: the curator's own module has no replace.
replace github.com/ZacxDev/clankmail v0.0.0 => github.com/ZacxDev/clankmail v0.0.0-20261101080649-c65f52f77152
