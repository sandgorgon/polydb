package polydb_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/sandgorgon/polydb"
)

// A database in memory: define a kind, add entities, query them.
func ExampleOpenMemory() {
	ctx := context.Background()
	db, err := polydb.OpenMemory(nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(ctx, `
		define kind Person { name: text, age: int };
		create Person { name: "Alice", age: 34 };
		create Person { name: "Bob", age: 27 };
		create Person { name: "Carol", age: 41 }`, nil)
	if err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `find Person p | where p.age > $min | sort p.name | show p.name, p.age`,
		polydb.Params{"min": 30})
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var age int
		if err := rows.Scan(&name, &age); err != nil {
			log.Fatal(err)
		}
		fmt.Println(name, age)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	// Output:
	// Alice 34
	// Carol 41
}

// A database in a directory keeps its data: open it again and it is there.
func ExampleOpen() {
	ctx := context.Background()
	dir, _ := os.MkdirTemp("", "polydb-example")
	defer os.RemoveAll(dir)

	db, err := polydb.Open(dir, nil)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `define kind Note { text: text }; create Note { text: "remember me" }`, nil); err != nil {
		log.Fatal(err)
	}
	db.Close()

	db, err = polydb.Open(dir, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(ctx, `find Note n | show n.text`, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		rows.Scan(&text)
		fmt.Println(text)
	}
	// Output: remember me
}

// Relations link entities into a graph that queries follow.
func ExampleOpenMemory_relations() {
	ctx := context.Background()
	db, _ := polydb.OpenMemory(nil)
	defer db.Close()

	_, err := db.Exec(ctx, `
		define kind Person { name: text };
		define kind Project { title: text };
		define relation works_on from Person to Project;
		create Person @ana { name: "Ana" };
		create Person @ben { name: "Ben" };
		create Project @db { title: "Database" };
		create Project @web { title: "Website" };
		relate @ana ~works_on~> @db;
		relate @ben ~works_on~> @db;
		relate @ben ~works_on~> @web`, nil)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := db.Query(ctx, `find Person p | follow p ~works_on~> Project j | where j.title = "Database" | sort p.name | show p.name`, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		rows.Scan(&name)
		fmt.Println(name, "works on the database")
	}
	// Output:
	// Ana works on the database
	// Ben works on the database
}

// A transaction's statements all take effect, or none do. RetryTx runs
// one again if it conflicts with another.
func ExampleRetryTx() {
	ctx := context.Background()
	db, _ := polydb.OpenMemory(nil)
	defer db.Close()
	db.Exec(ctx, `define kind Account { owner: text, balance: int };
		create Account { owner: "a", balance: 100 }; create Account { owner: "b", balance: 0 }`, nil)

	err := polydb.RetryTx(ctx, db, func(tx polydb.Tx) error {
		if _, err := tx.Exec(ctx, `find Account x | where x.owner = "a" | update x set balance = x.balance - 40`, nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `find Account x | where x.owner = "b" | update x set balance = x.balance + 40`, nil)
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
	rows, _ := db.Query(ctx, `find Account x | sort x.owner | show x.owner, x.balance`, nil)
	defer rows.Close()
	for rows.Next() {
		var owner string
		var balance int
		rows.Scan(&owner, &balance)
		fmt.Println(owner, balance)
	}
	// Output:
	// a 60
	// b 40
}

// A large value streams in from a reader and back out to a writer,
// never held whole in memory.
func ExampleEmbedded_Upload() {
	ctx := context.Background()
	db, _ := polydb.OpenMemory(nil)
	defer db.Close()
	db.Exec(ctx, `define kind Doc { name: text, body: text }`, nil)

	// Any io.Reader: a file, a network stream, ...
	content := strings.NewReader(strings.Repeat("All work and no play. ", 100_000))
	body, err := db.Upload(ctx, content, "text", "")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(ctx, `create Doc { name: "big", body: $body }`, polydb.Params{"body": body}); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(ctx, `find Doc d | show d.body, len(d.body)`, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var stored *polydb.Blob
		var n int
		if err := rows.Scan(&stored, &n); err != nil {
			log.Fatal(err)
		}
		read, err := io.Copy(io.Discard, stored.Reader()) // or to a file
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(n, "characters,", read, "bytes read back")
	}
	// Output: 2200000 characters, 2200000 bytes read back
}
