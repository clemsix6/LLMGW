# LLMGW — pivot CLIProxyAPI : état d'avancement

Dernière mise à jour : 2026-07-29
Branche : `feature/cliproxy-sdk-pivot` — jamais poussée, jamais mergée.

## En deux phrases

LLMGW ne fait plus tourner son propre code de proxy : il embarque CLIProxyAPI
comme bibliothèque et se concentre sur ce qui lui appartient — une clé d'API par
projet, des budgets, et la comptabilité de la consommation. Le chantier est
fonctionnellement terminé et entièrement vert.

## Ce qui reste à faire avant de basculer les projets dessus

1. **Un essai contre un vrai fournisseur.** Toute la validation actuelle est
   hermétique : vrai SDK, mais fournisseurs simulés. Rien n'a jamais tourné
   contre un vrai compte. À faire avec des identifiants de test isolés, jamais
   ceux de production :
   `LLMGW_LIVE_CONFIG=/chemin/vers/config-test.yaml just test-live`
2. **La bascule elle-même**, trafic arrêté : sauvegarde PostgreSQL testée,
   migration, premier démarrage, import des authentifications, création des
   clés projet et des budgets, puis bascule des clients.

Le retour arrière après migration exige de restaurer la sauvegarde : la
migration `0010` renomme les anciennes tables et supprime `reservation`.
Redéployer seulement l'ancienne image ne suffit pas.

## Décisions structurantes

- **Un projet est identifié uniquement par sa clé.** Pas d'en-tête spécifique,
  pas de port ni de domaine dédié. Les clients utilisent leur réglage habituel
  d'API key, ce qui les rend compatibles sans configuration particulière.
- **Plusieurs clés par projet**, typiquement une par client ou déploiement, pour
  révoquer finement. Les tags ont disparu.
- **Le SDK est embarqué**, pas lancé à côté : un seul binaire, un seul
  conteneur, un seul port.
- **Le SDK n'est jamais modifié.** Il est consommé tel quel depuis les sources
  officielles. Voir la section suivante.
- **Les clés restent obligatoires derrière Cloudflare** : le tunnel protège le
  transport, la clé porte l'identité du projet.
- **Pas de rechargement à chaud** : un changement de configuration exige un
  redémarrage. Une modification hostile du disque ne peut donc pas ouvrir de
  brèche à chaud.
- **Un seul serveur actif par base**, garanti par un verrou PostgreSQL.
- **PostgreSQL est obligatoire** et le service refuse de démarrer sans lui.

## Le fork CLIProxyAPI a été supprimé

Une version antérieure de ce chantier recopiait tout CLIProxyAPI dans le dépôt
(5 256 fichiers) et en modifiait 8 à la main, pour ne pas perdre le comptage
d'un cas particulier lié à la génération d'images sous Codex.

Cette approche a été abandonnée : CLIProxyAPI publie environ une version par
jour, et rejouer un correctif maison à chaque montée de version n'est pas
tenable.

Vérification faite dans le code du SDK, ce correctif ne servait à rien avec
notre configuration : `disable-image-generation` retire l'outil de génération
d'images de toutes les requêtes, sur tous les points d'entrée, et les règles
`payload` qui pourraient le réinjecter sont déjà refusées au démarrage. Le cas
protégé était donc inatteignable. Ces deux garde-fous remplacent le fork et
sont vérifiés au lancement.

Conséquence : mettre à jour le SDK est désormais une montée de version normale,
sans rien à réappliquer.

## Corrections apportées après la première exécution

- **Un arrêt inattendu du proxy pouvait figer le service indéfiniment**, en
  gardant le verrou PostgreSQL et la connexion à la base. Il pouvait aussi
  passer pour un arrêt normal, empêchant tout redémarrage automatique.
- **Une panne de comptabilité rendait le service muet** : plus aucune requête
  n'aboutissait, mais la page de santé répondait toujours « tout va bien » et
  rien ne redémarrait. Le service s'arrête maintenant pour être relancé propre,
  et l'état est visible sur la page de santé.
- **L'authentification échouait si une écriture de statistique échouait.** Cette
  écriture ne sert qu'au suivi ; elle ne bloque plus l'accès.
- **La release aurait échoué** : elle appelait un script supprimé.
- Le contexte de construction Docker embarquait `.env` et `.git`.
- Les règles de qualité partagées du dépôt Skills sont désormais importées, et
  un `Justfile` aligne le projet sur les autres.

## Comment valider

```
just test          # tout ce que la CI vérifie
just test-live     # essai réel, sur identifiants de test isolés
just docker-verify # image de production et ses contrôles
```

## Limites connues, assumées

- **Concurrence** : 64 générations simultanées par défaut, réglable via
  `usage-outstanding-capacity`. Au-delà, le service répond `503`.
- **Budgets** : chaque requête agrège la fenêtre complète sous un verrou par
  projet. Correct, mais coûteux si l'historique devient volumineux sur une
  fenêtre `day`. À revoir avec des compteurs si le besoin apparaît.
- Les budgets portant un tag ne sont pas repris par la migration, les tags
  n'existant plus.
- Les documents `2026-07-27-*` (plan et conception) sont des archives : leurs
  passages sur le fork du SDK sont caducs.
