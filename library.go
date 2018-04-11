package books

import (
	"database/sql"
	"database/sql/driver"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
)

var initialSchema = `create table books (
id integer primary key,
created_on timestamp not null default (datetime()),
updated_on timestamp not null default (datetime()),
author text not null,
series text,
title text not null,
extension text not null,
tags text,
original_filename text not null,
filename text not null,
file_size integer not null,
file_mtime timestamp not null,
hash text not null unique,
regexp_name text not null,
template_override text,
source text
);

create virtual table books_fts using fts4 (author, series, title, extension, tags,  filename, source);
`

func init() {
	// Add a connect hook to set synchronous = off for all connections.
	// This improves performance, especially during import,
	// but since changes aren't immediately synced to disk, data could be lost during a power outage or sudden OS crash.
	sql.Register("sqlite3async",
		&sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				conn.Exec("pragma synchronous=off", []driver.Value{})
				return nil
			},
		})
}

// Library represents a set of books in persistant storage.
type Library struct {
	*sql.DB
	filename  string
	booksRoot string
}

// OpenLibrary opens a library stored in a file.
func OpenLibrary(filename, booksRoot string) (*Library, error) {
	db, err := sql.Open("sqlite3async", filename)
	if err != nil {
		return nil, err
	}
	return &Library{db, filename, booksRoot}, nil
}

// CreateLibrary initializes a new library in the specified file.
// Once CreateLibrary is called, the file will be ready to open and accept new books.
// Warning: This function sets up a new library for the first time. To get a Library based on an existing library file,
// call OpenLibrary.
func CreateLibrary(filename string) error {
	log.Printf("Creating library in %s\n", filename)
	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return errors.Wrap(err, "Create library")
	}
	defer db.Close()

	_, err = db.Exec(initialSchema)
	if err != nil {
		return errors.Wrap(err, "Create library")
	}

	log.Printf("Library created in %s\n", filename)
	return nil
}

// ImportBook adds a book to a library.
// The file referred to by book.OriginalFilename will either be copied or moved to the location referred to by book.CurrentFilename, relative to the configured books root.
// The book will not be imported if another book already in the library has the same hash.
func (lib *Library) ImportBook(book Book, move bool) error {
	tx, err := lib.Begin()
	if err != nil {
		return err
	}

	rows, err := lib.Query("select id from books where hash=?", book.Hash)
	if err != nil {
		tx.Rollback()
		return err
	}
	if rows.Next() {
		// This book's hash is already in the library.
		var id int64
		rows.Scan(&id)
		tx.Rollback()
		return errors.Errorf("A duplicate book already exists with id %d", id)
	}

	rows.Close()
	if rows.Err() != nil {
		tx.Rollback()
		return errors.Wrapf(err, "Searching for duplicate book by hash %s", book.Hash)
	}

	tags := strings.Join(book.Tags, "/")
	res, err := tx.Exec(`insert into books (author, series, title, extension, tags, original_filename, filename, file_size, file_mtime, hash, regexp_name, source)
	values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		book.Author, book.Series, book.Title, book.Extension, tags, book.OriginalFilename, book.CurrentFilename, book.FileSize, book.FileMtime, book.Hash, book.RegexpName, book.Source)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Inserting book into the db")
	}

	id, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Fetching new book ID")
	}
	book.Id = id

	// Index book for searching.
	res, err = tx.Exec(`insert into books_fts (docid, author, series, title, extension, tags,  filename, source)
	values (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, book.Author, book.Series, book.Title, book.Extension, tags, book.CurrentFilename, book.Source)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Indexing book for search")
	}

	err = lib.moveOrCopyFile(book, move)
	if err != nil {
		tx.Rollback()
		return errors.Wrap(err, "Moving or copying book")
	}

	tx.Commit()
	log.Printf("Imported book: %s: %s, ID = %d", book.Author, book.Title, book.Id)

	return nil
}

// moveOrCopyFile moves or copies a file from book.OriginalFilename to book.CurrentFilename, relative to the configured books root.
// All necessary directories to make the destination valid will be created.
func (lib *Library) moveOrCopyFile(book Book, move bool) error {
	newName := book.CurrentFilename
	newPath := path.Join(lib.booksRoot, newName)
	err := os.MkdirAll(path.Dir(newPath), 0755)
	if err != nil {
		return err
	}

	if move {
		err = moveFile(book.OriginalFilename, newPath)
	} else {
		err = copyFile(book.OriginalFilename, newPath)
	}
	if err != nil {
		return err
	}

	return nil
}

// Search searches the library for books.
// By default, all fields are searched, but
// field:terms+to+search will limit to that field only.
// Fields: author, title, series, extension, tags, filename, source.
// Example: author:Stephen+King title:Shining
func (lib *Library) Search(terms string) ([]Book, error) {
	results := []Book{}
	rows, err := lib.Query("select docid from books_fts where books_fts match ?", terms)
	if err != nil {
		return results, errors.Wrap(err, "Querying db for search terms")
	}

	var ids []int64
	var id int64
	for rows.Next() {
		rows.Scan(&id)
		ids = append(ids, id)
	}
	err = rows.Err()
	if err != nil {
		return results, errors.Wrap(err, "Retreiving search results from db")
	}

	results, err = lib.GetBooksById(ids)
	if err != nil {
		return results, err
	}

	return results, nil
}

// GetBooksById retrieves books from the library by their id.
func (lib *Library) GetBooksById(ids []int64) ([]Book, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	results := []Book{}
	joined := strings.Repeat("?,", len(ids))
	joined = joined[:len(joined)-1]
	iids := make([]interface{}, 0)
	for _, id := range ids {
		iids = append(iids, id)
	}

	rows, err := lib.Query("select id, hash, author, series, tags, title, extension, filename from books where id in ("+joined+")", iids...)
	if err != nil {
		return results, errors.Wrap(err, "fetching books from database by ID")
	}

	for rows.Next() {
		book := Book{}
		var tags string
		if err := rows.Scan(&book.Id, &book.Hash, &book.Author, &book.Series, &tags, &book.Title, &book.Extension, &book.CurrentFilename); err != nil {
			return results, errors.Wrap(err, "scanning rows")
		}

		book.Tags = strings.Split(tags, "/")
		results = append(results, book)
	}

	if rows.Err() != nil {
		return results, errors.Wrap(err, "querying books by ID")
	}

	return results, nil
}

// ConvertToEpub converts a book to epub, and caches it in LIBRARY_ROOT/cache.
// This depends on ebook-convert, which takes the original filename, and the new filename, in that order.
// the book's current hash, with the extension .epub, will be the name of the cached file.
func (lib *Library) ConvertToEpub(book Book) error {
	filename := path.Join(lib.booksRoot, book.CurrentFilename)
	cacheDir := path.Join(path.Dir(lib.filename), "cache")
	newFile := path.Join(cacheDir, book.Hash+".epub")
	cmd := exec.Command("ebook-convert", filename, newFile)
	if err := cmd.Run(); err != nil {
		return err
	}

	return nil
}

// copyFile copies a file from src to dst, setting dst's modified time to that of src.
func copyFile(src, dst string) (e error) {
	fp, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, "Copy file")
	}
	defer fp.Close()

	st, err := fp.Stat()
	if err != nil {
		return errors.Wrap(err, "Copy file")
	}

	fd, err := os.Create(dst)
	if err != nil {
		return errors.Wrap(err, "Copy file")
	}
	defer func() {
		if err := fd.Close(); err != nil {
			e = errors.Wrap(err, "Copy file")
		}
		_ = os.Chtimes(dst, time.Now(), st.ModTime())
	}()

	if _, err := io.Copy(fd, fp); err != nil {
		return errors.Wrap(err, "Copy file")
	}

	log.Printf("Copied %s to %s", src, dst)

	return nil
}

// moveFile moves a file from src to dst.
// First, moveFile will attempt to rename the file,
// and if that fails, it will perform a copy and delete.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		err = copyFile(src, dst)
		if err != nil {
			return err
		}
		err = os.Remove(src)
		if err != nil {
			log.Printf("Error removing %s: %s", src, err)
			return nil
		}

		log.Printf("Moved %s to %s (copy/delete)", src, dst)
		return nil
	}

	log.Printf("Moved %s to %s", src, dst)
	return nil
}

// GetUniqueName checks to see if a file named f already exists, and if so, finds a unique name.
func GetUniqueName(f string) string {
	i := 1
	ext := path.Ext(f)
	newName := f
	_, err := os.Stat(newName)
	for err == nil {
		newName = strings.TrimSuffix(f, ext) + " (" + strconv.Itoa(i) + ")" + ext
		i++
		_, err = os.Stat(newName)
	}
	return newName
}
