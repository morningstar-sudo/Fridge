package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"

	"fridge/internal/dumpcheck"
)

// lastBytes is an io.Writer that keeps only the final n bytes it was given.
type lastBytes struct {
	n int
	b []byte
}

func (l *lastBytes) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	if len(l.b) > l.n {
		l.b = append(l.b[:0], l.b[len(l.b)-l.n:]...)
	}
	return len(p), nil
}

// checkOther reads a whole backup file of an engine other than PostgreSQL and says whether it is complete: the head must
// look right (dumpcheck.InspectOther) and the end must be there too, which is what a truncated upload lacks.
func checkOther(ctx context.Context, s Spec, objs Objects, head []byte, res map[string]any) error {
	hdr, err := dumpcheck.InspectOther(s.Engine, s.Key, head)
	if err != nil {
		return err
	}
	res["format"] = hdr.Format
	if hdr.ServerVersion != "" {
		res["source_version"] = hdr.ServerVersion
	}
	rc, err := objs.Get(ctx, s.Key)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	in := &countReader{r: rc}
	defer func() { res["bytes"] = in.n }()

	switch s.Engine {
	case "redis": // an RDB ends with the EOF opcode 0xFF and an 8-byte checksum
		var end lastBytes
		end.n = 9
		if _, err := io.Copy(&end, in); err != nil {
			return err
		}
		if len(end.b) < 9 || end.b[0] != 0xff {
			return errors.New("the RDB file is cut off (no end marker)")
		}
	case "mongo": // reading a gzip stream to its end verifies its checksum; the archive is inside
		zr, err := gzip.NewReader(in)
		if err != nil {
			return fmt.Errorf("not a gzip file: %w", err)
		}
		defer zr.Close()
		n, err := io.Copy(io.Discard, zr)
		if err != nil {
			return fmt.Errorf("the archive is corrupt or cut off: %w", err)
		}
		res["archive_bytes"] = n
	case "mysql", "mariadb":
		var r io.Reader = in
		if hdr.Gzip {
			zr, err := gzip.NewReader(in)
			if err != nil {
				return fmt.Errorf("not a gzip file: %w", err)
			}
			defer zr.Close()
			r = zr
		}
		var tc dumpcheck.TextChecker
		end := lastBytes{n: 4096}
		if _, err := io.Copy(io.MultiWriter(&tc, &end), r); err != nil {
			return err
		}
		if err := tc.Close(); err != nil {
			return err
		}
		if hdr.Gzip {
			res["sql_bytes"] = tc.N
		}
		if !bytes.Contains(end.b, []byte(dumpcheck.MySQLDumpEnd)) {
			return errors.New("the dump is cut off (no \"" + dumpcheck.MySQLDumpEnd + "\" at its end)")
		}
	}
	return nil
}
