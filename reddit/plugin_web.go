package reddit

import (
	log "github.com/Sirupsen/logrus"
	"github.com/jonas747/discordgo"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/web"
	"goji.io"
	"goji.io/pat"
	"golang.org/x/net/context"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

type CtxKey int

const (
	CurrentConfig CtxKey = iota
)

type Form struct {
	Subreddit string `schema:"subreddit" valid:",1,100"`
	Channel   string `schema:"channel" valid:"channel,false`
	ID        int    `schema:"id"`
}

func (p *Plugin) InitWeb() {
	web.Templates = template.Must(web.Templates.ParseFiles("templates/plugins/reddit.html"))

	redditMux := goji.SubMux()
	web.CPMux.HandleC(pat.New("/reddit/*"), redditMux)
	web.CPMux.HandleC(pat.New("/reddit"), redditMux)

	// Alll handlers here require guild channels present
	redditMux.UseC(web.RequireGuildChannelsMiddleware)
	redditMux.UseC(baseData)

	redditMux.HandleC(pat.Get("/"), web.RenderHandler(HandleReddit, "cp_reddit"))
	redditMux.HandleC(pat.Get(""), web.RenderHandler(HandleReddit, "cp_reddit"))

	// If only html forms allowed patch and delete.. if only
	redditMux.HandleC(pat.Post(""), web.FormParserMW(web.RenderHandler(HandleNew, "cp_reddit"), Form{}))
	redditMux.HandleC(pat.Post("/"), web.FormParserMW(web.RenderHandler(HandleNew, "cp_reddit"), Form{}))
	redditMux.HandleC(pat.Post("/:item/update"), web.FormParserMW(web.RenderHandler(HandleModify, "cp_reddit"), Form{}))
	redditMux.HandleC(pat.Post("/:item/delete"), web.FormParserMW(web.RenderHandler(HandleRemove, "cp_reddit"), Form{}))
}

// Adds the current config to the context
func baseData(inner goji.Handler) goji.Handler {
	mw := func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		client, activeGuild, templateData := web.GetBaseCPContextData(ctx)
		templateData["VisibleURL"] = "/cp/" + activeGuild.ID + "/reddit/"

		currentConfig, err := GetConfig(client, "guild_subreddit_watch:"+activeGuild.ID)
		if web.CheckErr(templateData, err, "Failed retrieving config, message support in the yagpdb server", log.Error) {
			web.LogIgnoreErr(web.Templates.ExecuteTemplate(w, "cp_reddit", templateData))
		}

		inner.ServeHTTPC(context.WithValue(ctx, CurrentConfig, currentConfig), w, r)

	}

	return goji.HandlerFunc(mw)
}

func HandleReddit(ctx context.Context, w http.ResponseWriter, r *http.Request) interface{} {
	_, _, templateData := web.GetBaseCPContextData(ctx)

	currentConfig := ctx.Value(CurrentConfig).([]*SubredditWatchItem)
	templateData["RedditConfig"] = currentConfig

	return templateData
}

func HandleNew(ctx context.Context, w http.ResponseWriter, r *http.Request) interface{} {
	client, activeGuild, templateData := web.GetBaseCPContextData(ctx)

	currentConfig := ctx.Value(CurrentConfig).([]*SubredditWatchItem)

	templateData["RedditConfig"] = currentConfig

	newElem := ctx.Value(common.ContextKeyParsedForm).(*Form)
	ok := ctx.Value(common.ContextKeyFormOk).(bool)
	if !ok {
		return templateData
	}

	// get an id the ez (and not safe if 2 people create at same time) way
	highest := 0
	for _, v := range currentConfig {
		if v.ID > highest {
			highest = v.ID
		}
	}

	if len(currentConfig) > 24 {
		return templateData.AddAlerts(web.ErrorAlert("Max 25 items allowed"))
	}

	watchItem := &SubredditWatchItem{
		Sub:     strings.TrimSpace(newElem.Subreddit),
		Channel: newElem.Channel,
		Guild:   activeGuild.ID,
		ID:      highest + 1,
	}

	err := watchItem.Set(client)
	if web.CheckErr(templateData, err, "Failed saving item :'(", log.Error) {
		return templateData
	}

	currentConfig = append(currentConfig, watchItem)
	templateData["RedditConfig"] = currentConfig
	templateData.AddAlerts(web.SucessAlert("Sucessfully added subreddit feed for /r/" + watchItem.Sub))

	// Log
	user := ctx.Value(common.ContextKeyUser).(*discordgo.User)
	go common.AddCPLogEntry(user, activeGuild.ID, "Added reddit feed from /r/"+newElem.Subreddit)
	return templateData
}

func HandleModify(ctx context.Context, w http.ResponseWriter, r *http.Request) interface{} {
	client, activeGuild, templateData := web.GetBaseCPContextData(ctx)

	currentConfig := ctx.Value(CurrentConfig).([]*SubredditWatchItem)
	templateData["RedditConfig"] = currentConfig

	updated := ctx.Value(common.ContextKeyParsedForm).(*Form)
	ok := ctx.Value(common.ContextKeyFormOk).(bool)
	if !ok {
		return templateData
	}
	updated.Subreddit = strings.TrimSpace(updated.Subreddit)

	item := FindWatchItem(currentConfig, updated.ID)
	if item == nil {
		return templateData.AddAlerts(web.ErrorAlert("Unknown id"))
	}

	subIsNew := !strings.EqualFold(updated.Subreddit, item.Sub)
	item.Channel = updated.Channel

	var err error
	if !subIsNew {
		// Pretty simple then
		err = item.Set(client)
	} else {
		err = item.Remove(client)
		if err == nil {
			item.Sub = strings.ToLower(r.FormValue("subreddit"))
			err = item.Set(client)
		}
	}

	if web.CheckErr(templateData, err, "Failed saving item :'(", log.Error) {
		return templateData
	}

	templateData.AddAlerts(web.SucessAlert("Sucessfully updated reddit feed! :D"))

	user := ctx.Value(common.ContextKeyUser).(*discordgo.User)
	common.AddCPLogEntry(user, activeGuild.ID, "Modified a feed to /r/"+r.FormValue("subreddit"))
	return templateData
}

func HandleRemove(ctx context.Context, w http.ResponseWriter, r *http.Request) interface{} {
	client, activeGuild, templateData := web.GetBaseCPContextData(ctx)

	currentConfig := ctx.Value(CurrentConfig).([]*SubredditWatchItem)
	templateData["RedditConfig"] = currentConfig

	id := pat.Param(ctx, "item")
	idInt, err := strconv.ParseInt(id, 10, 32)
	if err != nil {
		return templateData.AddAlerts(web.ErrorAlert("Failed parsing id", err))
	}

	// Get tha actual watch item from the config
	item := FindWatchItem(currentConfig, int(idInt))

	if item == nil {
		return templateData.AddAlerts(web.ErrorAlert("Unknown id"))
	}

	err = item.Remove(client)
	if web.CheckErr(templateData, err, "Failed removing item :'(", log.Error) {
		return templateData
	}

	templateData.AddAlerts(web.SucessAlert("Sucessfully removed subreddit feed for /r/ :')", item.Sub))

	// Remove it form the displayed list
	for k, c := range currentConfig {
		if c.ID == int(idInt) {
			currentConfig = append(currentConfig[:k], currentConfig[k+1:]...)
		}
	}

	templateData["RedditConfig"] = currentConfig

	user := ctx.Value(common.ContextKeyUser).(*discordgo.User)
	go common.AddCPLogEntry(user, activeGuild.ID, "Removed feed from /r/"+item.Sub)
	return templateData
}
