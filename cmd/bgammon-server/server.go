package main

import (
	"bytes"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.rocket9labs.com/tslocum/bgammon"
)

const clientTimeout = 10 * time.Minute

var onlyNumbers = regexp.MustCompile(`^[0-9]+$`)

type serverCommand struct {
	client  *serverClient
	command []byte
}

type server struct {
	clients      []*serverClient
	games        []*serverGame
	listeners    []net.Listener
	newGameIDs   chan int
	newClientIDs chan int
	commands     chan serverCommand

	gamesLock   sync.RWMutex
	clientsLock sync.Mutex
}

func newServer() *server {
	const bufferSize = 10
	s := &server{
		newGameIDs:   make(chan int),
		newClientIDs: make(chan int),
		commands:     make(chan serverCommand, bufferSize),
	}
	go s.handleNewGameIDs()
	go s.handleNewClientIDs()
	go s.handleCommands()
	go s.handleTerminatedGames()
	return s
}

func (s *server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	const bufferSize = 8
	commands := make(chan []byte, bufferSize)
	events := make(chan []byte, bufferSize)

	wsClient := newWebSocketClient(r, w, commands, events)
	if wsClient == nil {
		return
	}

	now := time.Now().Unix()

	c := &serverClient{
		id:         <-s.newClientIDs,
		account:    -1,
		connected:  now,
		lastActive: now,
		commands:   commands,
		Client:     wsClient,
	}
	s.handleClient(c)
}

func (s *server) listenWebSocket(address string) {
	log.Printf("Listening for WebSocket connections on %s...", address)
	err := http.ListenAndServe(address, http.HandlerFunc(s.handleWebSocket))
	log.Fatalf("failed to listen on %s: %s", address, err)
}

func (s *server) listen(network string, address string) {
	if strings.ToLower(network) == "ws" {
		go s.listenWebSocket(address)
		return
	}

	log.Printf("Listening for %s connections on %s...", strings.ToUpper(network), address)
	listener, err := net.Listen(network, address)
	if err != nil {
		log.Fatalf("failed to listen on %s: %s", address, err)
	}
	go s.handleListener(listener)
	s.listeners = append(s.listeners, listener)
}

func (s *server) handleListener(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatalf("failed to accept connection: %s", err)
		}
		go s.handleConnection(conn)
	}
}

func (s *server) nameAvailable(username []byte) bool {
	lower := bytes.ToLower(username)
	for _, c := range s.clients {
		if bytes.Equal(bytes.ToLower(c.name), lower) {
			return false
		}
	}
	return true
}

func (s *server) addClient(c *serverClient) {
	s.clientsLock.Lock()
	defer s.clientsLock.Unlock()

	s.clients = append(s.clients, c)
}

func (s *server) removeClient(c *serverClient) {
	go func() {
		g := s.gameByClient(c)
		if g != nil {
			g.removeClient(c)
		}
		c.Terminate("")
	}()

	s.clientsLock.Lock()
	defer s.clientsLock.Unlock()

	for i, sc := range s.clients {
		if sc == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			return
		}
	}
}

func (s *server) handleTerminatedGames() {
	t := time.NewTicker(time.Minute)
	for range t.C {
		s.gamesLock.Lock()

		i := 0
		for _, g := range s.games {
			if !g.terminated() {
				s.games[i] = g
				i++
			}
		}
		for j := i; j < len(s.games); j++ {
			s.games[j] = nil // Allow memory to be deallocated.
		}
		s.games = s.games[:i]

		s.gamesLock.Unlock()
	}
}

func (s *server) handleClient(c *serverClient) {
	s.addClient(c)

	log.Printf("Client %s connected", c.label())

	go s.handlePingClient(c)
	go s.handleClientCommands(c)

	c.HandleReadWrite()

	// Remove client.
	s.removeClient(c)

	log.Printf("Client %s disconnected", c.label())
}

func (s *server) handleConnection(conn net.Conn) {
	const bufferSize = 8
	commands := make(chan []byte, bufferSize)
	events := make(chan []byte, bufferSize)

	now := time.Now().Unix()

	c := &serverClient{
		id:         <-s.newClientIDs,
		account:    -1,
		connected:  now,
		lastActive: now,
		commands:   commands,
		Client:     newSocketClient(conn, commands, events),
	}
	s.sendHello(c)
	s.handleClient(c)
}

func (s *server) handlePingClient(c *serverClient) {
	// TODO only ping when there is no recent activity
	t := time.NewTicker(time.Minute * 4)
	for {
		<-t.C

		if c.Terminated() {
			t.Stop()
			return
		}

		if len(c.name) == 0 {
			c.Terminate("User did not send login command within 2 minutes.")
			t.Stop()
			return
		}

		c.lastPing = time.Now().Unix()
		c.sendEvent(&bgammon.EventPing{
			Message: fmt.Sprintf("%d", c.lastPing),
		})
	}
}

func (s *server) handleClientCommands(c *serverClient) {
	var command []byte
	for command = range c.commands {
		s.commands <- serverCommand{
			client:  c,
			command: command,
		}
	}
}

func (s *server) handleNewGameIDs() {
	gameID := 1
	for {
		s.newGameIDs <- gameID
		gameID++
	}
}

func (s *server) handleNewClientIDs() {
	clientID := 1
	for {
		s.newClientIDs <- clientID
		clientID++
	}
}

func (s *server) randomUsername() []byte {
	for {
		i := 100 + rand.Intn(900)
		name := []byte(fmt.Sprintf("Guest%d", i))

		if s.nameAvailable(name) {
			return name
		}
	}
}

func (s *server) sendHello(c *serverClient) {
	c.Write([]byte("hello Welcome to bgammon.org! Please log in by sending the 'login' command. You may specify a username, otherwise you will be assigned a random username. If you specify a username, you may also specify a password. Have fun!"))
}

func (s *server) gameByClient(c *serverClient) *serverGame {
	s.gamesLock.RLock()
	defer s.gamesLock.RUnlock()

	for _, g := range s.games {
		if g.client1 == c || g.client2 == c {
			return g
		}
	}
	return nil
}

func (s *server) handleCommands() {
	var cmd serverCommand
COMMANDS:
	for cmd = range s.commands {
		if cmd.client == nil {
			log.Panicf("nil client with command %s", cmd.command)
		}

		cmd.command = bytes.TrimSpace(cmd.command)

		firstSpace := bytes.IndexByte(cmd.command, ' ')
		var keyword string
		var startParameters int
		if firstSpace == -1 {
			keyword = string(cmd.command)
			startParameters = len(cmd.command)
		} else {
			keyword = string(cmd.command[:firstSpace])
			startParameters = firstSpace + 1
		}
		if keyword == "" {
			continue
		}
		keyword = strings.ToLower(keyword)
		params := bytes.Fields(cmd.command[startParameters:])

		// Require users to send login command first.
		if cmd.client.account == -1 {
			if keyword == bgammon.CommandLogin || keyword == bgammon.CommandLoginJSON || keyword == "l" || keyword == "lj" {
				if keyword == bgammon.CommandLoginJSON || keyword == "lj" {
					cmd.client.json = true
				}

				s.clientsLock.Lock()

				var username []byte
				var password []byte
				readUsername := func() bool {
					username = params[0]
					if onlyNumbers.Match(username) {
						cmd.client.Terminate("Invalid username: must contain at least one non-numeric character.")
						return false
					} else if !s.nameAvailable(username) {
						cmd.client.Terminate("Username unavailable.")
						return false
					}
					return true
				}
				switch len(params) {
				case 0:
					username = s.randomUsername()
				case 1:
					if !readUsername() {
						s.clientsLock.Unlock()
						continue
					}
				default:
					if !readUsername() {
						s.clientsLock.Unlock()
						continue
					}
					password = bytes.Join(params[1:], []byte(" "))
				}

				s.clientsLock.Unlock()

				if len(password) > 0 {
					cmd.client.account = 1
				} else {
					cmd.client.account = 0
				}
				cmd.client.name = username

				cmd.client.sendEvent(&bgammon.EventWelcome{
					PlayerName: string(cmd.client.name),
					Clients:    len(s.clients),
					Games:      len(s.games),
				})

				log.Printf("Client %d logged in as %s", cmd.client.id, cmd.client.name)
				continue
			}

			cmd.client.Terminate("You must login before using other commands.")
			continue
		}

		clientGame := s.gameByClient(cmd.client)

		switch keyword {
		case bgammon.CommandHelp, "h":
			// TODO get extended help by specifying a command after help
			cmd.client.sendEvent(&bgammon.EventHelp{
				Topic:   "",
				Message: "Test help text",
			})
		case bgammon.CommandJSON:
			sendUsage := func() {
				cmd.client.sendNotice("To enable JSON formatted messages, send 'json on'. To disable JSON formatted messages, send 'json off'.")
			}
			if len(params) != 1 {
				sendUsage()
				continue
			}
			paramLower := strings.ToLower(string(params[0]))
			switch paramLower {
			case "on":
				cmd.client.json = true
				cmd.client.sendNotice("JSON formatted messages enabled.")
			case "off":
				cmd.client.json = false
				cmd.client.sendNotice("JSON formatted messages disabled.")
			default:
				sendUsage()
			}
		case bgammon.CommandSay, "s":
			if len(params) == 0 {
				continue
			}
			if clientGame == nil {
				cmd.client.sendNotice("Message not sent: You are not currently in a match.")
				continue
			}
			opponent := clientGame.opponent(cmd.client)
			if opponent == nil {
				cmd.client.sendNotice("Message not sent: There is no one else in the match.")
				continue
			}
			ev := &bgammon.EventSay{
				Message: string(bytes.Join(params, []byte(" "))),
			}
			ev.Player = string(cmd.client.name)
			opponent.sendEvent(ev)
		case bgammon.CommandList, "ls":
			ev := &bgammon.EventList{}

			s.gamesLock.RLock()
			for _, g := range s.games {
				if g.terminated() {
					continue
				}
				ev.Games = append(ev.Games, bgammon.GameListing{
					ID:       g.id,
					Password: len(g.password) != 0,
					Players:  g.playerCount(),
					Name:     string(g.name),
				})
			}
			s.gamesLock.RUnlock()

			cmd.client.sendEvent(ev)
		case bgammon.CommandCreate, "c":
			sendUsage := func() {
				cmd.client.sendNotice("To create a public match please specify whether it is public or private. When creating a private match, a password must also be provided.")
			}
			if len(params) == 0 {
				sendUsage()
				continue
			}
			var gamePassword []byte
			gameType := bytes.ToLower(params[0])
			var gameName []byte
			switch {
			case bytes.Equal(gameType, []byte("public")):
				gameName = bytes.Join(params[1:], []byte(" "))
			case bytes.Equal(gameType, []byte("private")):
				if len(params) < 2 {
					sendUsage()
					continue
				}
				gamePassword = params[1]
				gameName = bytes.Join(params[2:], []byte(" "))
			default:
				sendUsage()
				continue
			}

			// Set default game name.
			if len(bytes.TrimSpace(gameName)) == 0 {
				abbr := "'s"
				lastLetter := cmd.client.name[len(cmd.client.name)-1]
				if lastLetter == 's' || lastLetter == 'S' {
					abbr = "'"
				}
				gameName = []byte(fmt.Sprintf("%s%s match", cmd.client.name, abbr))
			}

			g := newServerGame(<-s.newGameIDs)
			g.name = gameName
			g.password = gamePassword
			ok, reason := g.addClient(cmd.client)
			if !ok {
				log.Panicf("failed to add client to newly created game %+v %+v: %s", g, cmd.client, reason)
			}

			s.gamesLock.Lock()
			s.games = append(s.games, g)
			s.gamesLock.Unlock()
		case bgammon.CommandJoin, "j":
			if clientGame != nil {
				cmd.client.sendEvent(&bgammon.EventFailedJoin{
					Reason: "Please leave the match you are in before joining another.",
				})
				continue
			}

			sendUsage := func() {
				cmd.client.sendNotice("To join a match please specify its ID or the name of a player in the match. To join a private match, a password must also be specified.")
			}

			if len(params) == 0 {
				sendUsage()
				continue
			}

			var joinGameID int
			if onlyNumbers.Match(params[0]) {
				gameID, err := strconv.Atoi(string(params[0]))
				if err == nil && gameID > 0 {
					joinGameID = gameID
				}

				if joinGameID == 0 {
					sendUsage()
					continue
				}
			} else {
				paramLower := bytes.ToLower(params[0])
				s.clientsLock.Lock()
				for _, sc := range s.clients {
					if bytes.Equal(paramLower, bytes.ToLower(sc.name)) {
						g := s.gameByClient(sc)
						if g != nil {
							joinGameID = g.id
						}
						break
					}
				}
				s.clientsLock.Unlock()

				if joinGameID == 0 {
					cmd.client.sendEvent(&bgammon.EventFailedJoin{
						Reason: "Match not found.",
					})
					continue
				}
			}

			s.gamesLock.Lock()
			for _, g := range s.games {
				if g.terminated() {
					continue
				}
				if g.id == joinGameID {
					if len(g.password) != 0 && (len(params) < 2 || !bytes.Equal(g.password, bytes.Join(params[2:], []byte(" ")))) {
						cmd.client.sendEvent(&bgammon.EventFailedJoin{
							Reason: "Invalid password.",
						})
						s.gamesLock.Unlock()
						continue COMMANDS
					}

					ok, reason := g.addClient(cmd.client)
					if !ok {
						cmd.client.sendEvent(&bgammon.EventFailedJoin{
							Reason: reason,
						})
					}
					s.gamesLock.Unlock()
					continue COMMANDS
				}
			}
			s.gamesLock.Unlock()

			cmd.client.sendEvent(&bgammon.EventFailedJoin{
				Reason: "Match not found.",
			})
		case bgammon.CommandLeave, "l":
			if clientGame == nil {
				cmd.client.sendEvent(&bgammon.EventFailedLeave{
					Reason: "You are not currently in a match.",
				})
				continue
			}

			clientGame.removeClient(cmd.client)
		case bgammon.CommandRoll, "r":
			if clientGame == nil {
				cmd.client.sendEvent(&bgammon.EventFailedRoll{
					Reason: "You are not currently in a match.",
				})
				continue
			}

			if !clientGame.roll(cmd.client.playerNumber) {
				cmd.client.sendEvent(&bgammon.EventFailedRoll{
					Reason: "It is not your turn to roll.",
				})
				continue
			}

			ev := &bgammon.EventRolled{
				Roll1: clientGame.Roll1,
				Roll2: clientGame.Roll2,
			}
			ev.Player = string(cmd.client.name)
			if clientGame.Turn == 0 && clientGame.Roll1 != 0 && clientGame.Roll2 != 0 {
				if clientGame.Roll1 > clientGame.Roll2 {
					clientGame.Turn = 1
				} else if clientGame.Roll2 > clientGame.Roll1 {
					clientGame.Turn = 2
				} else {
					clientGame.Roll1 = 0
					clientGame.Roll2 = 0
				}
			}
			clientGame.eachClient(func(client *serverClient) {
				client.sendEvent(ev)
				if clientGame.Turn != 0 || !client.json {
					clientGame.sendBoard(client)
				}
			})
		case bgammon.CommandMove, "m", "mv":
			if clientGame == nil {
				cmd.client.sendEvent(&bgammon.EventFailedMove{
					Reason: "You are not currently in a match.",
				})
				continue
			}

			if clientGame.Turn != cmd.client.playerNumber {
				cmd.client.sendEvent(&bgammon.EventFailedMove{
					Reason: "It is not your turn to move.",
				})
				continue
			}

			sendUsage := func() {
				cmd.client.sendEvent(&bgammon.EventFailedMove{
					Reason: "Specify one or more moves in the form FROM/TO. For example: 8/4 6/4",
				})
			}

			if len(params) == 0 {
				sendUsage()
				continue
			}

			var moves [][]int
			for i := range params {
				split := bytes.Split(params[i], []byte("/"))
				if len(split) != 2 {
					sendUsage()
					continue COMMANDS
				}
				from := bgammon.ParseSpace(string(split[0]))
				if from == -1 {
					sendUsage()
					continue COMMANDS
				}
				to := bgammon.ParseSpace(string(split[1]))
				if to == -1 {
					sendUsage()
					continue COMMANDS
				}

				if !bgammon.ValidSpace(from) || !bgammon.ValidSpace(to) {
					cmd.client.sendEvent(&bgammon.EventFailedMove{
						From:   from,
						To:     to,
						Reason: "Illegal move.",
					})
					continue COMMANDS
				}

				from, to = bgammon.FlipSpace(from, cmd.client.playerNumber), bgammon.FlipSpace(to, cmd.client.playerNumber)
				moves = append(moves, []int{from, to})
			}

			ok, expandedMoves := clientGame.AddMoves(moves)
			if !ok {
				cmd.client.sendEvent(&bgammon.EventFailedMove{
					From:   0,
					To:     0,
					Reason: "Illegal move.",
				})
				continue
			}

			var winEvent *bgammon.EventWin
			if clientGame.Winner != 0 {
				winEvent = &bgammon.EventWin{}
				if clientGame.Winner == 1 {
					winEvent.Player = clientGame.Player1.Name
				} else {
					winEvent.Player = clientGame.Player2.Name
				}
			}

			clientGame.eachClient(func(client *serverClient) {
				ev := &bgammon.EventMoved{
					Moves: bgammon.FlipMoves(expandedMoves, client.playerNumber),
				}
				ev.Player = string(cmd.client.name)
				client.sendEvent(ev)

				clientGame.sendBoard(client)

				if winEvent != nil {
					client.sendEvent(winEvent)
				}
			})
		case bgammon.CommandReset:
			if clientGame == nil {
				cmd.client.sendNotice("You are not currently in a match.")
				continue
			}

			if clientGame.Turn != cmd.client.playerNumber {
				cmd.client.sendNotice("It is not your turn.")
				continue
			}

			if len(clientGame.Moves) == 0 {
				continue
			}

			l := len(clientGame.Moves)
			undoMoves := make([][]int, l)
			for i, move := range clientGame.Moves {
				undoMoves[l-1-i] = []int{move[1], move[0]}
			}
			ok, _ := clientGame.AddMoves(undoMoves)
			if !ok {
				cmd.client.sendNotice("Failed to undo move: invalid move.")
			} else {
				clientGame.eachClient(func(client *serverClient) {
					ev := &bgammon.EventMoved{
						Moves: bgammon.FlipMoves(undoMoves, client.playerNumber),
					}
					ev.Player = string(cmd.client.name)

					client.sendEvent(ev)
					clientGame.sendBoard(client)
				})
			}
		case bgammon.CommandOk, "k":
			if clientGame == nil {
				cmd.client.sendNotice("You are not currently in a match.")
				continue
			}

			legalMoves := clientGame.LegalMoves()
			if len(legalMoves) != 0 {
				available := bgammon.FlipMoves(legalMoves, cmd.client.playerNumber)
				bgammon.SortMoves(available)
				cmd.client.sendEvent(&bgammon.EventFailedOk{
					Reason: fmt.Sprintf("The following legal moves are available: %s", bgammon.FormatMoves(available)),
				})
				continue
			}

			clientGame.NextTurn()
			clientGame.eachClient(func(client *serverClient) {
				clientGame.sendBoard(client)
			})
		case bgammon.CommandRematch, "rm":
			if clientGame == nil {
				cmd.client.sendNotice("You are not currently in a match.")
				continue
			} else if clientGame.Winner == 0 {
				cmd.client.sendNotice("The match you are in is still in progress.")
				continue
			} else if clientGame.rematch == cmd.client.playerNumber {
				cmd.client.sendNotice("You have already requested a rematch.")
				continue
			} else if clientGame.client1 == nil || clientGame.client2 == nil {
				cmd.client.sendNotice("Your opponent left the match.")
				continue
			} else if clientGame.rematch != 0 && clientGame.rematch != cmd.client.playerNumber {
				s.gamesLock.Lock()

				newGame := newServerGame(<-s.newGameIDs)
				newGame.name = clientGame.name
				newGame.password = clientGame.password
				newGame.client1 = clientGame.client1
				newGame.client2 = clientGame.client2
				newGame.Player1 = clientGame.Player1
				newGame.Player2 = clientGame.Player2
				s.games = append(s.games, newGame)

				clientGame.client1 = nil
				clientGame.client2 = nil

				s.gamesLock.Unlock()

				ev1 := &bgammon.EventJoined{
					GameID:       newGame.id,
					PlayerNumber: 1,
				}
				ev1.Player = newGame.Player1.Name

				ev2 := &bgammon.EventJoined{
					GameID:       newGame.id,
					PlayerNumber: 2,
				}
				ev2.Player = newGame.Player2.Name

				newGame.eachClient(func(client *serverClient) {
					client.sendEvent(ev1)
					client.sendEvent(ev2)
					newGame.sendBoard(client)
				})
			} else {
				clientGame.rematch = cmd.client.playerNumber

				clientGame.opponent(cmd.client).sendNotice("Your opponent would like to play again. Type /rematch to accept.")
				cmd.client.sendNotice("Rematch offer sent.")
				continue
			}
		case bgammon.CommandBoard, "b":
			if clientGame == nil {
				cmd.client.sendNotice("You are not currently in a match.")
				continue
			}

			clientGame.sendBoard(cmd.client)
		case bgammon.CommandDisconnect:
			if clientGame != nil {
				clientGame.removeClient(cmd.client)
			}
			cmd.client.Terminate("Client disconnected")
		case bgammon.CommandPong:
			// Do nothing.

			// TODO remove
		case "endgame":
			if clientGame == nil {
				cmd.client.sendNotice("You are not currently in a match.")
				continue
			}

			clientGame.Turn = 1
			clientGame.Roll1 = 1
			clientGame.Roll2 = 2
			clientGame.Board = []int{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, -1, 0, 0, 0}

			clientGame.eachClient(func(client *serverClient) {
				clientGame.sendBoard(client)
			})
		default:
			log.Printf("Received unknown command from client %s: %s", cmd.client.label(), cmd.command)
		}
	}
}
