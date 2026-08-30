package sshkey

import (
	"encoding/pem"
)

func encodePEM(b *pem.Block) []byte { return pem.EncodeToMemory(b) }
