// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os

import (
	"internal/poll"
	"io"
	"syscall"
)

var (
	pollCopyFileRange = poll.CopyFileRange
	pollSplice        = poll.Splice
)

func (f *File) readFrom(r io.Reader) (written int64, handled bool, err error) {
	// Neither copy_file_range(2) nor splice(2) supports destinations opened with
	// O_APPEND, so don't bother to try zero-copy with these system calls.
	//
	// Visit https://man7.org/linux/man-pages/man2/copy_file_range.2.html#ERRORS and
	// https://man7.org/linux/man-pages/man2/splice.2.html#ERRORS for details.
	if f.appendMode {
		return 0, false, nil
	}

	written, handled, err = f.copyFileRange(r)
	if handled {
		return
	}
	return f.spliceToFile(r)
}

func (f *File) spliceToFile(r io.Reader) (written int64, handled bool, err error) {
	// At least as of kernel 5.19.11, splice to a tty fails.
	// poll.Splice will do the wrong thing if it can splice from r
	// but can't splice to f: it will read data from r, which is
	// not what we want if r is a pipe or socket.
	// So we have to check now whether f is a tty.
	fi, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	if fi.Mode()&ModeCharDevice != 0 {
		return 0, false, nil
	}

	var (
		remain int64
		lr     *io.LimitedReader
	)
	if lr, r, remain = tryLimitedReader(r); remain <= 0 {
		return 0, true, nil
	}

	pfd := getPollFD(r)
	// TODO(panjf2000): run some tests to see if we should unlock the non-streams for splice.
	// Streams benefit the most from the splice(2), non-streams are not even supported in old kernels
	// where splice(2) will just return EINVAL; newer kernels support non-streams like UDP, but I really
	// doubt that splice(2) could help non-streams, cuz they usually send small frames respectively
	// and one splice call would result in one frame.
	// splice(2) is suitable for large data but the generation of fragments defeats its edge here.
	// Therefore, don't bother to try splice if the r is not a streaming descriptor.
	if pfd == nil || !pfd.IsStream {
		return
	}

	var syscallName string
	written, handled, syscallName, err = pollSplice(&f.pfd, pfd, remain)

	if lr != nil {
		lr.N = remain - written
	}

	return written, handled, wrapSyscallError(syscallName, err)
}

// getPollFD tries to get the poll.FD from the given io.Reader by expecting
// the underlying type of r to be the implementation of syscall.Conn that contains
// a *net.rawConn.
func getPollFD(r io.Reader) *poll.FD {
	sc, ok := r.(syscall.Conn)
	if !ok {
		return nil
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil
	}
	ipfd, ok := rc.(interface{ PollFD() *poll.FD })
	if !ok {
		return nil
	}
	return ipfd.PollFD()
}

func (f *File) copyFileRange(r io.Reader) (written int64, handled bool, err error) {
	var (
		remain int64
		lr     *io.LimitedReader
	)
	if lr, r, remain = tryLimitedReader(r); remain <= 0 {
		return 0, true, nil
	}

	src, ok := r.(*File)
	if !ok {
		return 0, false, nil
	}
	if src.checkValid("ReadFrom") != nil {
		// Avoid returning the error as we report handled as false,
		// leave further error handling as the responsibility of the caller.
		return 0, false, nil
	}

	written, handled, err = pollCopyFileRange(&f.pfd, &src.pfd, remain)
	if lr != nil {
		lr.N -= written
	}
	return written, handled, wrapSyscallError("copy_file_range", err)
}

// tryLimitedReader tries to assert the io.Reader to io.LimitedReader, it returns the io.LimitedReader,
// the underlying io.Reader and the remaining amount of bytes if the assertion succeeds,
// otherwise it just returns the original io.Reader and the theoretical unlimited remaining amount of bytes.
func tryLimitedReader(r io.Reader) (*io.LimitedReader, io.Reader, int64) {
	var remain int64 = 1<<63 - 1 // by default, copy until EOF

	lr, ok := r.(*io.LimitedReader)
	if !ok {
		return nil, r, remain
	}

	remain = lr.N
	return lr, lr.R, remain
}
