package sstable

import "math/bits"

// The bloom filter: a bit array followed by one byte, the number of
// probes. Probes use double hashing from one 64-bit hash, which must
// never change: filters are stored on disk.

func bloomHash(key []byte) uint64 {
	h := uint64(14695981039346656037) // FNV-1a
	for _, c := range key {
		h ^= uint64(c)
		h *= 1099511628211
	}
	// splitmix64's finalizer spreads FNV's weak low bits.
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	return h ^ h>>31
}

func buildFilter(hashes []uint64, bitsPerKey int) []byte {
	nbits := max(64, len(hashes)*bitsPerKey)
	nbits = (nbits + 7) / 8 * 8
	k := min(max(bitsPerKey*69/100, 1), 30) // ln 2 × bits per key
	f := make([]byte, nbits/8+1)
	for _, h := range hashes {
		delta := bits.RotateLeft64(h, 31)
		for range k {
			b := h % uint64(nbits)
			f[b/8] |= 1 << (b % 8)
			h += delta
		}
	}
	f[len(f)-1] = byte(k)
	return f
}

// mayContain reports whether key may be in the filter; false is certain.
func mayContain(f []byte, key []byte) bool {
	if len(f) < 2 {
		return true
	}
	k := int(f[len(f)-1])
	if k > 30 || k == 0 {
		return true // a format this version doesn't know
	}
	nbits := uint64(len(f)-1) * 8
	h := bloomHash(key)
	delta := bits.RotateLeft64(h, 31)
	for range k {
		b := h % nbits
		if f[b/8]&(1<<(b%8)) == 0 {
			return false
		}
		h += delta
	}
	return true
}
