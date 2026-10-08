package codecs

// AC3 is a AC-3 codec.
type AC3 struct {
	SampleRate   int
	ChannelCount int
}

// IsVideo returns whether the codec is a video one.
func (*AC3) IsVideo() bool {
	return false
}

func (*AC3) isCodec() {
}
