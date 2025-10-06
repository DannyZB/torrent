package common

import (
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
)

func LengthIterFromUpvertedFiles(fis []metainfo.FileInfo) segments.LengthIter {
	return func(yield func(segments.Length) bool) {
		for _, fi := range fis {
			if !yield(segments.Length(fi.Length)) {
				return
			}
		}
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
