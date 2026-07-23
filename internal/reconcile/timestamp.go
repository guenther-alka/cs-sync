package reconcile

import "time"

// nowStamp formats the current time for conflict filenames, matching the
// YYYY.MM.DD_HH.MM.SS format used elsewhere in cs-sync/csweb-gui docs.
// A var (not const) so tests can override it.
var nowStamp = func() string {
	return time.Now().Format("2006.01.02_15.04.05")
}
