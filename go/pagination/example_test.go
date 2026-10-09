package pagination_test

import (
	"fmt"
	"net/url"
	"sort"

	"github.com/Arif9878/common/go/pagination"
)

type Order struct{ ID string }

// orders stands in for a table with an index on id.
var orders = []Order{{"o-1"}, {"o-2"}, {"o-3"}, {"o-4"}, {"o-5"}}

// listAfter is the keyset query: SELECT … WHERE id > $1 ORDER BY id LIMIT $2.
func listAfter(after string, limit int) []Order {
	i := sort.Search(len(orders), func(i int) bool { return orders[i].ID > after })
	return orders[i:min(i+limit, len(orders))]
}

func Example() {
	cursors := pagination.NewCursor[string]()
	query := url.Values{"limit": {"2"}}
	for {
		p, err := pagination.FromQuery(query)
		if err != nil {
			panic(err)
		}
		after, err := cursors.Decode(p.Cursor)
		if err != nil {
			panic(err)
		}
		page, err := pagination.NewPage(listAfter(after, p.Limit+1), p.Limit, cursors, func(o Order) string { return o.ID })
		if err != nil {
			panic(err)
		}
		fmt.Println(page.Items, page.NextCursor != "")
		if page.NextCursor == "" {
			break
		}
		query.Set("cursor", page.NextCursor) // the client sends it back
	}
	// Output:
	// [{o-1} {o-2}] true
	// [{o-3} {o-4}] true
	// [{o-5}] false
}
