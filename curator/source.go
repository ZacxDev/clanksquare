package curator

import "context"

// fetchSource reads one source of any kind into a Feed. Every kind yields the
// same Item shape, so what happens next (filters, dedupe, caps, posting) is
// shared and kind-blind.
func (r *runner) fetchSource(ctx context.Context, ch *Channel, fc FeedConfig) (*Feed, error) {
	if fc.Kind == KindHFDaily {
		return r.fetchHFDaily(ctx, ch, fc)
	}
	f, err := FetchFeed(ctx, r.Client, fc.URL)
	if err == nil && fc.TitleFromSummary {
		titlesFromSummaries(f)
	}
	return f, err
}

// maxDerivedTitle bounds a title made from a summary; the whole text still
// goes in the excerpt.
const maxDerivedTitle = 140

// titlesFromSummaries gives every untitled item the start of its summary as
// its title.
func titlesFromSummaries(f *Feed) {
	for i := range f.Items {
		if f.Items[i].Title == "" {
			f.Items[i].Title = Truncate(f.Items[i].Summary, maxDerivedTitle)
		}
	}
}
