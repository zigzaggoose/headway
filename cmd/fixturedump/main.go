// Command fixturedump records live feed responses into testdata/ and derives
// the edge-case fixtures from them.
//
// Tests never call the live API: they are deterministic, they run offline, and
// they consume no quota in CI (§11.2). This is the one place that does, run by
// hand when a fixture needs refreshing.
//
//	fixturedump capture -n 2 -interval 15s
//	fixturedump derive
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/zigzaggoose/headway/internal/config"
	"github.com/zigzaggoose/headway/internal/feed"
	"github.com/zigzaggoose/headway/internal/gtfsrt"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: fixturedump capture|derive [flags]"))
	}
	var err error
	switch os.Args[1] {
	case "capture":
		err = capture(os.Args[2:])
	case "derive":
		err = derive(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q: want capture or derive", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// capture fetches n consecutive responses interval apart. Two of them is the
// dedupe fixture: the same trips, seconds later, mostly unchanged, which is
// exactly what the change filter has to suppress.
func capture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	n := fs.Int("n", 2, "how many consecutive responses to record")
	interval := fs.Duration("interval", 15*time.Second, "wait between responses")
	feedID := fs.String("feed", "sydneytrains", "feed id from the catalogue")
	outDir := fs.String("out", "testdata", "directory to write into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var target config.Feed
	for _, f := range cfg.Feeds {
		if f.ID == *feedID {
			target = f
		}
	}
	if target.ID == "" {
		return fmt.Errorf("feed %q is not in the resolved catalogue; check HEADWAY_ENABLED_FEEDS", *feedID)
	}

	client := feed.NewClient(cfg.APIKey, cfg.Poll.HTTPTimeout, time.Now)
	ctx := context.Background()

	for i := 1; i <= *n; i++ {
		if i > 1 {
			fmt.Printf("waiting %s for the feed to move on\n", *interval)
			time.Sleep(*interval)
		}
		resp, err := client.Fetch(ctx, target.ID, target.RealtimeURL, feed.Conditional{})
		if err != nil {
			return fmt.Errorf("capture %d: %w", i, err)
		}
		decoded, err := gtfsrt.Decode(resp.FeedID, resp.Body, resp.FetchedAt)
		if err != nil {
			return fmt.Errorf("capture %d is not decodable, refusing to save it: %w", i, err)
		}

		name := filepath.Join(*outDir, fmt.Sprintf("%s_tripupdate_%04d.pb", target.ID, i))
		if err := os.WriteFile(name, resp.Body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		fmt.Printf("%s: %d bytes, %d entities, %d updates, header %s (%s old)\n",
			name, len(resp.Body), decoded.Entities, len(decoded.Updates),
			decoded.HeaderTS.Format(time.RFC3339), decoded.Age().Round(time.Second))
	}
	return nil
}

// derive builds the edge-case fixtures from a recorded one, so they can be
// rebuilt when the shape of the feed changes rather than being mystery bytes
// nobody dares touch. Each is trimmed to a handful of entities: the point is
// the shape, not the volume.
func derive(args []string) error {
	fs := flag.NewFlagSet("derive", flag.ExitOnError)
	src := fs.String("src", "testdata/sydneytrains_tripupdate_0001.pb", "recorded response to derive from")
	outDir := fs.String("out", "testdata", "directory to write into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	body, err := os.ReadFile(*src)
	if err != nil {
		return fmt.Errorf("read %s: %w", *src, err)
	}
	var msg gtfs.FeedMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("decode %s: %w", *src, err)
	}
	if len(msg.GetEntity()) < 3 {
		return fmt.Errorf("%s has only %d entities, too few to derive from", *src, len(msg.GetEntity()))
	}

	// A cancelled trip, as the feed usually sends one: the relationship and no
	// stop-time updates at all (§9.1 case 3).
	cancelled := trim(&msg, 3)
	e := cancelled.Entity[0]
	e.TripUpdate.Trip.ScheduleRelationship = gtfs.TripDescriptor_CANCELED.Enum()
	e.TripUpdate.StopTimeUpdate = nil

	// A cancelled trip that still lists its stops (§9.1 case 4).
	if len(cancelled.Entity) > 1 && len(cancelled.Entity[1].TripUpdate.StopTimeUpdate) > 0 {
		cancelled.Entity[1].TripUpdate.Trip.ScheduleRelationship = gtfs.TripDescriptor_CANCELED.Enum()
	}
	if err := write(filepath.Join(*outDir, "sydneytrains_cancelled.pb"), cancelled); err != nil {
		return err
	}

	// An added trip: no schedule row exists for it by definition, so it can
	// never match and must not count against the match rate (§9.1 case 2).
	added := trim(&msg, 2)
	added.Entity[0].TripUpdate.Trip.ScheduleRelationship = gtfs.TripDescriptor_ADDED.Enum()
	added.Entity[0].TripUpdate.Trip.TripId = proto.String("ADDED-TRIP-NOT-IN-ANY-SCHEDULE")
	if err := write(filepath.Join(*outDir, "sydneytrains_added.pb"), added); err != nil {
		return err
	}

	// A trip that started before midnight on the previous service date, whose
	// start time is past 24:00:00. This is the shape servicetime has to get
	// right (§9.2).
	past := trim(&msg, 2)
	past.Entity[0].TripUpdate.Trip.StartTime = proto.String("25:10:00")
	past.Entity[0].TripUpdate.Trip.StartDate = proto.String(time.Now().AddDate(0, 0, -1).Format("20060102"))
	if err := write(filepath.Join(*outDir, "sydneytrains_pastmidnight.pb"), past); err != nil {
		return err
	}

	// A body that starts like a feed and stops mid-message, which is what a
	// dropped connection produces. proto.Unmarshal must fail, not panic.
	truncated := body
	if len(truncated) > 200 {
		truncated = truncated[:200]
	}
	name := filepath.Join(*outDir, "malformed_truncated.pb")
	if err := os.WriteFile(name, truncated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	fmt.Printf("%s: %d bytes\n", name, len(truncated))
	return nil
}

// trim copies the header and the first n entities. Deep-copying matters: the
// derived fixtures are built one after another from the same source message.
func trim(msg *gtfs.FeedMessage, n int) *gtfs.FeedMessage {
	out := proto.Clone(msg).(*gtfs.FeedMessage)
	if len(out.Entity) > n {
		out.Entity = out.Entity[:n]
	}
	return out
}

func write(name string, msg *gtfs.FeedMessage) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := os.WriteFile(name, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	fmt.Printf("%s: %d bytes, %d entities\n", name, len(body), len(msg.GetEntity()))
	return nil
}
