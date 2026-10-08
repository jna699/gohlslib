package codecs

// MPEG1Audio is a MPEG-1 Audio codec.
type MPEG1Audio struct{}

// IsVideo returns whether the codec is a video one.
func (*MPEG1Audio) IsVideo() bool {
	return false
}

func (*MPEG1Audio) isCodec() {
}
