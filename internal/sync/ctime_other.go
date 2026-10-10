//go:build !linux && !darwin

package sync

import "io/fs"

// ctimeDe: o Windows não dá o ctime pelo fs.FileInfo; o atalho do estado
// local fica só com tamanho e data de modificação, e a reconferência rotativa
// cobre o resto.
func ctimeDe(fs.FileInfo) (int64, uint32) { return 0, 0 }
