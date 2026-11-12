package remote

const defaultEventChannelCapacity = 128

// A tool response may exceed bufio.Scanner's default 64 KiB line limit.
const maxSSELineBytes = 16 * 1024 * 1024

func firstElicitationID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
