// caisson contains the code used to index

package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"unsafe"
)

var currentBlockDocumentCount = 0
var bloomFilter []uint64
var currentDocumentCount = 0
var currentBlockStartDocumentCount = 0

// Search the results we need to look at very quickly using only bit operations
// mostly limited by memory access
func Search(queryBits []uint64) []uint32 {
	var results []uint32
	var res uint64

	if len(queryBits) == 0 {
		return results
	}

	// we want to go through the index, stepping though each "shard"
	for i := 0; i < len(bloomFilter); i += BloomSize {
		// preload the res with the result of the first queryBit and if it's not 0 then we continue
		// if it is 0 it means nothing can be a match so we don't need to do anything
		res = bloomFilter[queryBits[0]+uint64(i)]

		// we don't need to look at the first queryBit anymore so start at one
		// then go through each long looking to see if we keep a match anywhere
		for j := 1; j < len(queryBits); j++ {
			res = res & bloomFilter[queryBits[j]+uint64(i)]

			// if we have 0 meaning no bits set we should bail out because there is nothing more to do here
			// as we cannot have a match even if further queryBits have something set
			if res == 0 {
				break
			}
		}

		// if we have a non 0 value that means at least one bit is set indicating a match
		// so now we need to go through each bit and work out which document it is
		if res != 0 {
			for j := 0; j < DocumentsPerBlock; j++ {
				// determine which bits are still set indicating they have all the bits
				// set for this query which means we have a potential match
				if res&(1<<j) > 0 {
					results = append(results, uint32(DocumentsPerBlock*(i/BloomSize)+j))
				}
			}
		}

	}

	return results
}

// Tokenize returns a slice of tokens for the given text.
func Tokenize(text string) []Trigram {
	res := strings.Fields(strings.ToLower(text))
	var cres []string
	for _, v := range res {
		if len(v) >= 3 {
			cres = append(cres, v)
		}
	}

	// now we have clean tokens trigram them
	var trigrams []Trigram
	for _, r := range cres {
		trigrams = append(trigrams, Trigrams(r)...)
	}

	return trigrams
}

// Itemise given some content will turn it into tokens
// and then use those to create the bit positions we need to
// set for our bloomFilter filter index
func Itemise(tokens []Trigram) []bool {
	docBool := make([]bool, BloomSize)

	for _, token := range tokens {
		for _, i := range HashBloom(token.BytesFast()) {
			docBool[i] = true
		}
	}
	return docBool
}

type Trigram [3]rune

// Bytes is the simplest way to turn an array of runes into a slice of bytes
// that represent a UTF-8 string.
// There is a faster way to do this, but not needed for this demo.
// See: https://stackoverflow.com/questions/29255746/how-encode-rune-into-byte-using-utf8
func (t Trigram) Bytes() []byte {
	return []byte(string(t[:]))
}

// BytesFast returns the underlying bytes of the 3 runes. These bytes are
// suitable for hashing but the slice does not represent a UTF-8 string.
// Use .Bytes() if you want to encode the Trigram to a UTF-8 string.
func (t Trigram) BytesFast() []byte {
	return (*[12]byte)(unsafe.Pointer(&t))[:]
}

// Trigrams takes in text and returns its trigrams.
func Trigrams(text string) []Trigram {
	runes := []rune(text)

	// if we have less than or 2 runes we cannot do anything so bail out
	if len(runes) < 3 {
		return []Trigram{}
	}

	// allocate all trigrams
	ngrams := make([]Trigram, len(runes)-2)

	// create the trigrams
	for i := 0; i < len(runes)-2; i++ {
		ngrams[i] = Trigram(runes[i : i+3])
	}

	return ngrams
}

// Queryise given some content will turn it into tokens
// and then hash them and store the resulting values into
// a slice which we can use to query the bloom filter
func Queryise(query string) []uint64 {
	var queryBits []uint64
	for _, w := range Tokenize(query) {
		queryBits = append(queryBits, HashBloom(w.BytesFast())...)
	}

	// removing duplicates and sorting should in theory improve RAM access
	// and hence performance
	queryBits = RemoveUInt64Duplicates(queryBits)
	sort.Slice(queryBits, func(i, j int) bool {
		return queryBits[i] < queryBits[j]
	})

	return queryBits
}

// RemoveUInt64Duplicates removes duplicate values from uint64 slice
func RemoveUInt64Duplicates(s []uint64) []uint64 {
	if len(s) < 2 {
		return s
	}
	sort.Slice(s, func(x, y int) bool { return s[x] > s[y] })
	var e = 1
	for i := 1; i < len(s); i++ {
		if s[i] == s[i-1] {
			continue
		}
		s[e] = s[i]
		e++
	}

	return s[:e]
}

// HashBloom hashes a single token/word 3 times to give us the entry
// locations we need for our bloomFilter filter
func HashBloom(word []byte) []uint64 {
	var hashes []uint64

	h1 := fnv.New64a()
	h2 := fnv.New64()

	// 3 hashes is probably OK for our purposes
	// but to be really like Bing it should change this
	// based on how common/rare the term is where
	// rarer terms are hashes more

	_, _ = h1.Write(word)
	hashes = append(hashes, h1.Sum64()%BloomSize)
	h1.Reset()

	_, _ = h2.Write(word)
	hashes = append(hashes, h2.Sum64()%BloomSize)

	_, _ = h1.Write(word)
	_, _ = h1.Write([]byte("salt")) // anything works here
	hashes = append(hashes, h1.Sum64()%BloomSize)
	h1.Reset()

	return hashes
}

// Add adds items into the internal bloomFilter used later for pre-screening documents
// note that it fills the filter from right to left, which might not be what you expect
func Add(item []bool) error {
	// bailout if we ever get something that will break the index
	// because it does not match the size we expect
	if len(item) != BloomSize {
		return errors.New(fmt.Sprintf("expected to match size %d", BloomSize))
	}

	// we need to know if we need to add another batch to this index...
	// which should only be called if we are building from the start
	// or if we need to reset
	if currentBlockDocumentCount == 0 || currentBlockDocumentCount == DocumentsPerBlock {
		bloomFilter = append(bloomFilter, make([]uint64, BloomSize)...)
		currentBlockDocumentCount = 0

		// We don't want to do this for the first document, but everything after
		// we want to know the offset, so in short trail by 1 BloomSize
		if currentDocumentCount != 0 {
			currentBlockStartDocumentCount += BloomSize
		}
	}

	// we need to go through each item and set the correct bit
	for i, bit := range item {
		// if bit is set then we need to flip that bit from its default state, remember this fills from right to left
		// which is not what you expect possibly... anyway it does not matter which way it goes
		if bit {
			bloomFilter[currentBlockStartDocumentCount+i] |= 1 << currentBlockDocumentCount // 0 in this case is the bit we want to flip so it would be 1 if we added document 2 to this block
		}
	}

	// now we increment where we are and where the current block counts are so we can continue to add
	currentBlockDocumentCount++
	currentDocumentCount++

	return nil
}

// PrintIndex prints out the index which can be useful from time
// to time to ensure that bits are being set correctly.
func PrintIndex() {
	// display what the bloomFilter filter looks like broken into chunks
	for j, i := range bloomFilter {
		if j%BloomSize == 0 {
			fmt.Println("")
		}

		fmt.Printf("%064b\n", i)
	}
}

// Ngrams given input splits it according the requested size
// such that you can get trigrams or whatever else is required
func Ngrams(text string, size int) []string {
	var runes = []rune(text)

	var ngrams []string

	for i := 0; i < len(runes); i++ {
		if i+size < len(runes)+1 {
			ngram := runes[i : i+size]
			ngrams = append(ngrams, string(ngram))
		}
	}

	return ngrams
}

// GetFill returns the % value of how much this doc was filled, allowing for
// determining if the index will be overfilled for this document
func GetFill(doc []bool) float64 {
	count := 0
	for _, i := range doc {
		if i {
			count++
		}
	}

	return float64(count) / float64(BloomSize) * 100
}
