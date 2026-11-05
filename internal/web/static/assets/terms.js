"use strict";
// Terms of use and disclaimer shown in the web interface. Same text as the
// README ("Terms of use and disclaimer"); languages the interface doesn't
// have fall back to English. Bump TERMS_VERSION when the text changes, so
// it is shown again.
const TERMS_VERSION = "1";
const TERMS = {
  en: {
    title: "Terms of use and disclaimer",
    intro: "By using MyMule you accept these terms. If you don't accept them, don't use MyMule.",
    items: [
      ["What MyMule is.", "MyMule is free, open-source software that connects to the public eD2K network. It is a tool: it doesn't host, index, provide, control or endorse any content found on that network."],
      ["Lawful use only.", "You may only use MyMule to search, download, share or distribute content that is in the public domain, that is released under a license that allows it (for example free software or Creative Commons works), that you own, that you have permission from the rights holder to use, or whose download and sharing is allowed by the laws of your country. Downloading or sharing copyrighted material without permission may be illegal where you live."],
      ["You are responsible.", "You alone are responsible for what you search, download and share with MyMule, and for complying with the laws that apply to you (copyright, intellectual property, privacy and others). Note that eD2K is a public network: other users can see your IP address and the files you share, and by default MyMule shares your completed downloads and the verified parts of the files you are downloading."],
      ["No warranty.", "MyMule is provided \"as is\", without warranty of any kind, as stated in the GNU General Public License (sections 15 and 16)."],
      ["No liability.", "The authors and contributors of MyMule are not responsible for how it is used, for the content other users share on the network, or for any damage, loss or legal consequence resulting from its use."],
      ["Third parties.", "File names and search results come from other users of the network. Servers, TMDB, Wikidata and any other services MyMule talks to have their own terms; MyMule is not affiliated with or endorsed by any of them, nor by any rights holder."],
    ],
    outro: "This text is not legal advice. If you are unsure whether something is allowed where you live, don't download or share it.",
    accept: "I accept",
    decline: "I don't accept",
    close: "Close",
    link: "Terms of use",
  },
  es: {
    title: "Condiciones de uso y aviso legal",
    intro: "Al usar MyMule aceptas estas condiciones. Si no las aceptas, no uses MyMule.",
    items: [
      ["Qué es MyMule.", "MyMule es software libre y de código abierto que se conecta a la red pública eD2K. Es una herramienta: no aloja, indexa, ofrece, controla ni respalda ningún contenido de esa red."],
      ["Solo para un uso legal.", "Solo puedes usar MyMule para buscar, descargar, compartir o distribuir contenido de dominio público, publicado con una licencia que lo permita (por ejemplo software libre u obras Creative Commons), que sea tuyo, para el que tengas permiso del titular de los derechos, o cuya descarga y compartición permitan las leyes de tu país. Descargar o compartir material protegido por derechos de autor sin permiso puede ser ilegal donde vives."],
      ["La responsabilidad es tuya.", "Solo tú eres responsable de lo que buscas, descargas y compartes con MyMule, y de cumplir las leyes que te afecten (derechos de autor, propiedad intelectual, privacidad y otras). Ten en cuenta que eD2K es una red pública: otros usuarios pueden ver tu dirección IP y los ficheros que compartes, y por defecto MyMule comparte tus descargas terminadas y las partes ya verificadas de lo que estás descargando."],
      ["Sin garantía.", "MyMule se ofrece \"tal cual\", sin garantía de ningún tipo, como indica la Licencia Pública General de GNU (secciones 15 y 16)."],
      ["Sin responsabilidad.", "Los autores y colaboradores de MyMule no son responsables del uso que se haga de él, del contenido que otros usuarios comparten en la red, ni de ningún daño, pérdida o consecuencia legal derivada de su uso."],
      ["Terceros.", "Los nombres de los ficheros y los resultados de búsqueda vienen de otros usuarios de la red. Los servidores, TMDB, Wikidata y cualquier otro servicio con el que MyMule se comunica tienen sus propias condiciones; MyMule no está afiliado a ninguno de ellos ni respaldado por ellos, ni por ningún titular de derechos."],
    ],
    outro: "Este texto no es asesoramiento legal. Si no estás seguro de si algo está permitido donde vives, no lo descargues ni lo compartas.",
    accept: "Acepto",
    decline: "No acepto",
    close: "Cerrar",
    link: "Condiciones de uso",
  },
};
