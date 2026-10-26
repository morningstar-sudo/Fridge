package dumpcheck

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
)

// InspectOther vets the head of a backup file of an engine other than PostgreSQL. head may be shorter than the file.
//
//	redis:           an RDB file (starts REDIS0nnn)
//	mongo:           a gzip-compressed mongodump archive (the decompressed stream starts with the archive magic)
//	mysql, mariadb:  a (gzip-compressed) mysqldump text file
func InspectOther(eng, key string, head []byte) (Header, error) {
	if len(head) == 0 {
		return Header{}, errors.New("empty file")
	}
	switch eng {
	case "redis":
		if !strings.HasSuffix(key, ".rdb") {
			return Header{}, errors.New("unsupported file type (want .rdb)")
		}
		if len(head) < 9 || string(head[:5]) != "REDIS" {
			return Header{}, errors.New("not a Redis RDB file (missing REDIS signature)")
		}
		return Header{Format: "rdb"}, nil
	case "mongo":
		if !strings.HasSuffix(key, ".archive.gz") {
			return Header{}, errors.New("unsupported file type (want .archive.gz)")
		}
		zr, err := gzip.NewReader(bytes.NewReader(head))
		if err != nil {
			return Header{}, errors.New("not a gzip file")
		}
		var magic [4]byte
		if _, err := io.ReadFull(zr, magic[:]); err != nil {
			return Header{}, fmt.Errorf("not a mongodump archive: %w", err)
		}
		if magic != ArchiveMagic {
			return Header{}, errors.New("not a mongodump archive (missing archive magic)")
		}
		return Header{Format: "archive", Gzip: true}, nil
	case "mysql", "mariadb":
		gz := strings.HasSuffix(key, ".sql.gz")
		if !gz && !strings.HasSuffix(key, ".sql") {
			return Header{}, errors.New("unsupported file type (want .sql.gz or .sql)")
		}
		h, err := inspectSQLWith(head, gz, looksLikeMySQLDump, "mysqldump")
		if err != nil {
			return Header{}, err
		}
		h.ServerVersion = mysqlDumpVersion(head, gz)
		return h, nil
	}
	return Header{}, fmt.Errorf("no file checks for engine %q", eng)
}

// ArchiveMagic opens every mongodump archive (little-endian 0x8199e26d).
var ArchiveMagic = [4]byte{0x6d, 0xe2, 0x99, 0x81}

// MySQLDumpEnd is how a complete mysqldump/mariadb-dump file ends (the date is optional).
const MySQLDumpEnd = "-- Dump completed"

func looksLikeMySQLDump(text []byte) bool {
	for _, marker := range []string{"-- MySQL dump", "-- MariaDB dump", "/*!40101", "/*M!999999"} {
		if bytes.Contains(text, []byte(marker)) {
			return true
		}
	}
	return false
}

// mysqlDumpVersion reads the "-- Server version<TAB>8.4.0" line of the head ("" if absent).
func mysqlDumpVersion(head []byte, gz bool) string {
	text := head
	if gz {
		zr, err := gzip.NewReader(bytes.NewReader(head))
		if err != nil {
			return ""
		}
		text, _ = io.ReadAll(zr)
	}
	for _, line := range strings.Split(string(text), "\n") {
		if v, ok := strings.CutPrefix(line, "-- Server version"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
