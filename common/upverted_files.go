package common

import (
	"github.com/dannyzb/torrent/metainfo"
	"github.com/dannyzb/torrent/segments"
)

func LengthIterFromUpvertedFiles(fis []metainfo.FileInfo) segments.LengthIter {
	i := 0
	return func() (segments.Length, bool) {
		if i == len(fis) {
			return -1, false
		}
		l := fis[i].Length
		i++
		return l, true
	}
}

// Returns file segments, BitTorrent v2 aware.
func TorrentOffsetFileSegments(info *metainfo.Info) (ret []segments.Extent) {
	files := info.UpvertedFiles()
	for _, fi := range files {
		ret = append(ret, segments.Extent{fi.TorrentOffset, fi.Length})
	}
	return
}
