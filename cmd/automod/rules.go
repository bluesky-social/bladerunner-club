package main

import (
	"strings"

	appbsky "github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/bluesky-social/indigo/automod"
	"github.com/bluesky-social/indigo/automod/countstore"
	"github.com/bluesky-social/indigo/automod/helpers"
)

var (
	GOOD_BOT_THRESHOLD = 2
	BAD_BOT_THRESHOLD  = 3
	//BLADERUNNER_THRESHOLD = 2
	//JABRONI_THRESHOLD = 2
)

const (
	NilBot  = ""
	GoodBot = "good-bot"
	BadBot  = "bad-bot"
)

var _ automod.PostRuleFunc = GoodBotBadBotRule

// top-level rule for good-bot/bad-bot "voting" and auto-labeling system
func GoodBotBadBotRule(c *automod.RecordContext, post *appbsky.FeedPost) error {

	// is this post assessing a subject account as "good bot" or "bad bot"? if not, bail out early
	vote, subjectDID := parseBotAssessment(c, post)
	if vote == NilBot || subjectDID == nil {
		return nil
	}

	eng := c.InternalEngine()
	authorDID := c.Account.Identity.DID
	authorMeta, err := eng.GetAccountMeta(c.Ctx, c.Account.Identity)
	if err != nil {
		return err
	}

	// if the account is a jabroni or bad bot, bail out
	if accountHasLabel(authorMeta, "jabroni") || accountHasLabel(authorMeta, "bad-bot") {
		c.Logger.Warn("skipping bot assessment from bad account", "authorLabels", authorMeta.AccountLabels)
		return nil
	}

	// if we got this far, the assessment is legitimate
	// TODO: c.TagRecord("bot-assessment")

	subjectIdent, err := eng.Directory.LookupDID(c.Ctx, *subjectDID)
	if err != nil {
		return err
	}
	subjectMeta, err := eng.GetAccountMeta(c.Ctx, subjectIdent)
	if err != nil {
		return err
	}
	goodCount := c.GetCountDistinct("good-bot", subjectDID.String(), countstore.PeriodTotal)
	badCount := c.GetCountDistinct("bad-bot", subjectDID.String(), countstore.PeriodTotal)

	if vote == GoodBot {
		c.IncrementDistinct("good-bot", subjectDID.String(), authorDID.String())
		goodCount = goodCount + 1
	} else if vote == BadBot {
		c.IncrementDistinct("bad-bot", subjectDID.String(), authorDID.String())
		badCount = goodCount + 1
	}

	c.Logger.Warn("valid bot assessment", "vote", vote, "goodCount", goodCount, "badCount", badCount, "subjectLabels", subjectMeta.AccountLabels)

	// blade-runner authors auto-apply, regardless of counts
	if accountHasLabel(authorMeta, "bladerunner") {
		if err = addAccountLabel(c, *subjectDID, vote); err != nil {
			return err
		}
		// c.Notify("slack")
		return nil
	}

	if vote == GoodBot && goodCount >= GOOD_BOT_THRESHOLD {
		if err = addAccountLabel(c, *subjectDID, vote); err != nil {
			return err
		}
		// c.Notify("slack")
		return nil
	}

	if vote == BadBot && badCount >= BAD_BOT_THRESHOLD {
		if err = addAccountLabel(c, *subjectDID, vote); err != nil {
			return err
		}
		// c.Notify("slack")
		return nil
	}

	return nil
}

// parses the post, decideds if it is saying another account is a good/bad bot. returns the assessment type and the subject DID
func parseBotAssessment(c *automod.RecordContext, post *appbsky.FeedPost) (string, *syntax.DID) {

	vote := parsePostText(post.Text)
	if vote == NilBot {
		return vote, nil
	}

	// if this is a reply, the subject is the immediate parent
	if post.Reply != nil && !helpers.IsSelfThread(c, post) {
		parentURI, err := syntax.ParseATURI(post.Reply.Parent.Uri)
		if err != nil {
			return NilBot, nil
		}
		parentDID, err := parentURI.Authority().AsDID()
		if err != nil {
			return NilBot, nil
		}
		return vote, &parentDID
	}

	// if there is a single metion, the subject is the mentioned account
	facets, err := helpers.ExtractFacets(post)
	if err != nil {
		// just skip invalid posts
		return NilBot, nil
	}
	if len(facets) == 1 && facets[0].DID != nil {
		did, err := syntax.ParseDID(*facets[0].DID)
		if nil == err {
			return vote, &did
		}
	}

	// TODO: quote posts
	return NilBot, nil
}

func parsePostText(s string) string {
	// @TODO: this is a dumb check that only matches text exactly, could be improved
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "bad bot" {
		return BadBot
	}
	if s == "good bot" {
		return GoodBot
	}
	return NilBot
}
