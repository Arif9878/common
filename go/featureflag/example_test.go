package featureflag_test

import (
	"context"
	"fmt"

	"github.com/Arif9878/common/go/featureflag"
)

func ExampleStatic() {
	// In production: featureflag.NewOpenFeature(openfeature.NewClient("orders")).
	var flags featureflag.Evaluator = featureflag.Static{"checkout.new-pricing": true}

	ctx := context.Background()
	user := featureflag.Attributes{featureflag.TargetingKey: "user-7", "country": "ID"}
	on, err := flags.Bool(ctx, "checkout.new-pricing", false, user)
	fmt.Println(on, err)

	// Unknown flags return the default, with an error to log.
	on, err = flags.Bool(ctx, "checkout.unknown", false, user)
	fmt.Println(on, err != nil)
	// Output:
	// true <nil>
	// false true
}
