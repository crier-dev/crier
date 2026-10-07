package trust

import "encoding/hex"

// hexEncode is the hex form keys ride the wire in.
func hexEncode(b []byte) string { return hex.EncodeToString(b) }
